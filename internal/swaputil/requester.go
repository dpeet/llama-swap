package swaputil

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Requester is who sent a request, captured so the scheduler can log which
// caller triggered a model load and its evictions. Without it a surprise
// eviction leaves no trace of its requester (the 2026-10-04 incident).
type Requester struct {
	// Client is the best available identity: "ts:<login>" from tailscale
	// serve's Tailscale-User-Login header (user-owned devices only; tagged
	// devices get none), else "xff:<ip>" from X-Forwarded-For, else
	// "ip:<host>" from the TCP peer. Empty for internal requests (preload,
	// adopt), which have no peer.
	Client string
	// IP is the forwarded or peer address, kept beside a "ts:" Client because
	// the login names the user while the IP names the device.
	IP     string
	Method string
	// Path is the client's original path, before any handler rewrote it.
	Path      string
	UserAgent string
}

// RequesterFrom extracts r's requester. The tailscale and forwarded headers
// are client-supplied, so they identify a caller only when the request came
// through tailscale serve, which overwrites them; a loopback caller could set
// them itself.
func RequesterFrom(r *http.Request) Requester {
	forwarded := forwardedIP(r)
	ip := forwarded
	if ip == "" && r.RemoteAddr != "" {
		ip = r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			ip = host
		}
	}

	var client string
	switch login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); {
	case login != "":
		client = "ts:" + login
	case forwarded != "":
		client = "xff:" + ip
	case ip != "":
		client = "ip:" + ip
	}

	// RequestURI is the path as the client sent it, because handlers rewrite
	// URL.Path (e.g. /upstream/<model>/health becomes /health). The query is
	// dropped because it can carry secrets.
	path := r.URL.Path
	if r.RequestURI != "" {
		path, _, _ = strings.Cut(r.RequestURI, "?")
	}

	return Requester{
		Client:    client,
		IP:        ip,
		Method:    r.Method,
		Path:      path,
		UserAgent: r.Header.Get("User-Agent"),
	}
}

// LogFields formats the requester as the client=, ip=, method=, path= and ua=
// fields shared by the swap-start and unload-request log lines, with "internal"
// for a request that has no peer (preload, adopt). Values are quoted so a
// User-Agent with spaces stays one field and a newline cannot forge a line.
func (q Requester) LogFields() string {
	client := q.Client
	if client == "" {
		client = "internal"
	}
	return fmt.Sprintf("client=%q ip=%q method=%q path=%q ua=%q", client, q.IP, q.Method, q.Path, q.UserAgent)
}

// forwardedIP returns the first X-Forwarded-For hop, or "". X-Real-IP is not
// read because tailscale serve never sets it, so only a local caller could
// supply it, and it would outrank the real peer address.
func forwardedIP(r *http.Request) string {
	first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
	return strings.TrimSpace(first)
}
