NTLM transport module for Caddy's reverse proxy
===============================================

This plugin adds NTLM reverse proxying support to Caddy.

The `http_ntlm` transport is identical to the `http` transport, but the HTTP version is always 1.1, and Keep-Alive is always disabled.
