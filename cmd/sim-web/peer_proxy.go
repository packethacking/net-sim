package main

// peer_proxy.go — a tiny generic HTTP reverse proxy for "peer
// services" deployed alongside net-sim in the same Docker network.
//
// The motivating use case is the test-net chatbot: it runs in its own
// container on the rfnet network, exposes an HTTP control plane on
// port 8090, and the visualiser wants START/STOP buttons. Rather than
// hardcoding "chatbot" knowledge into sim-web, this file exposes a
// generic /api/peer/<name>/<path> handler that resolves <name> via
// Docker DNS and forwards the request. If the peer isn't reachable
// (no such container, wrong network, peer down) the response is a
// clean 503 so the visualiser can show a tidy "not present" state.

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// peerProxyAllowlist is the set of peer hostnames + ports we are
// willing to proxy to. Hardcoded rather than open so a misconfigured
// network can't be turned into an SSRF probe; add entries here as new
// peer services land.
//
// The map value is the upstream TCP port; the path passed by the
// client maps directly onto the upstream's URL path.
var peerProxyAllowlist = map[string]int{
	"chatbot": 8090,
}

// handlePeerProxy is mounted at /api/peer/. URL shape:
//
//	/api/peer/<name>/<path>   →  http://<name>:<port>/<path>
//
// Methods, headers, and body pass through. Response body and the
// upstream's Content-Type / Cache-Control headers are returned. If
// the upstream is unreachable the handler returns 503.
func (a *app) handlePeerProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/peer/")
	if rest == "" || rest == r.URL.Path {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	upPath := "/"
	if len(parts) == 2 {
		upPath += parts[1]
	}
	port, ok := peerProxyAllowlist[name]
	if !ok {
		http.Error(w, "unknown peer", http.StatusNotFound)
		return
	}

	// Short timeouts so an unreachable peer fails the request fast
	// (the visualiser polls /state regularly and treats 5xx as "peer
	// not present").
	cli := &http.Client{
		Timeout: 30 * time.Second,
	}
	upURL := "http://" + name + ":" + itoa(port) + upPath
	if r.URL.RawQuery != "" {
		upURL += "?" + r.URL.RawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, upURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Pass through only safe-ish headers.
	for _, h := range []string{"Content-Type", "Content-Length", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := cli.Do(req)
	if err != nil {
		// DNS / dial / timeout — peer not present from our point of
		// view. 503 is the right code; the visualiser hides the
		// associated UI on this.
		if isUnreachable(err) {
			http.Error(w, "peer not reachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "peer error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func itoa(n int) string {
	// Tiny inline to avoid pulling in strconv at module-import scope.
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// isUnreachable reports whether err looks like "the peer container
// isn't there" rather than "the peer returned 500 / bad response."
// Used to map down to a 503 vs. 502 in the proxy response so the
// visualiser can distinguish "no peer deployed" from "peer is broken."
func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errAs(err, &dnsErr) {
		return true
	}
	type timeouter interface{ Timeout() bool }
	if t, ok := err.(timeouter); ok && t.Timeout() {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "no such host") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "timeout")
}

// errAs is a tiny replacement for errors.As that handles the only
// type we need (net.DNSError) without pulling in the errors package
// for one use site. Falls through to nil on type mismatch.
func errAs(err error, target **net.DNSError) bool {
	if err == nil {
		return false
	}
	if d, ok := err.(*net.DNSError); ok {
		*target = d
		return true
	}
	return false
}
