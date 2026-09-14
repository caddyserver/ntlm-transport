package reverseproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// TestCleanupClosesBoundConnectionsWithoutDeadlock stops the transport while a
// client-bound upstream connection sits idle in its pool, which is the state
// after any NTLM/Negotiate round trip. Cleanup closes that connection, and the
// connection's Close hook unbinds the client from the same map Cleanup is
// working on, so Cleanup must not hold the map lock while closing.
func TestCleanupClosesBoundConnectionsWithoutDeadlock(t *testing.T) {
	var upstreamConnsClosed atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			upstreamConnsClosed.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	n := NTLMTransport{}.CaddyModule().New().(*NTLMTransport)
	if err := n.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// A request carrying NTLM/Negotiate credentials binds the client
	// connection (identified by RemoteAddr) to a dedicated upstream transport.
	// The dial hook of that transport reads the original request from the
	// context, as the reverse proxy provides it.
	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.RemoteAddr = "192.0.2.1:40000"
	req.Header.Set("Authorization", "Negotiate dGVzdA==")

	idle := make(chan struct{}, 1)
	trace := &httptrace.ClientTrace{
		PutIdleConn: func(error) { idle <- struct{}{} },
	}
	reqCtx := context.WithValue(req.Context(), caddyhttp.OriginalRequestCtxKey, *req)
	req = req.WithContext(httptrace.WithClientTrace(reqCtx, trace))

	resp, err := n.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing body: %v", err)
	}
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream connection was not returned to the idle pool")
	}

	n.transportsMu.RLock()
	bound := len(n.transports)
	n.transportsMu.RUnlock()
	if bound != 1 {
		t.Fatalf("expected 1 bound transport before Cleanup, got %d", bound)
	}

	done := make(chan error, 1)
	go func() { done <- n.Cleanup() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cleanup did not return within 5s: deadlock between Cleanup and unbinderConn.Close")
	}

	n.transportsMu.RLock()
	bound = len(n.transports)
	n.transportsMu.RUnlock()
	if bound != 0 {
		t.Fatalf("expected no bound transports after Cleanup, got %d", bound)
	}

	deadline := time.Now().Add(5 * time.Second)
	for upstreamConnsClosed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Cleanup did not close the idle upstream connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
