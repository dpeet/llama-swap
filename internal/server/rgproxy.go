package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// rgAPIURLEnv names the env var holding rg-api's base URL. It is an env var
// rather than a config key because a config key needs a schema entry and a KB
// guide, and this fork-only proxy has neither.
const rgAPIURLEnv = "LLAMA_SWAP_RG_API_URL"

// rgResponseHeaderTimeout is 10 minutes because rg-api's slowest single action,
// a release, is bounded at 300 s (rg.sh down) + 180 s (rg-hold release) = 480 s,
// and a grab at 60 s + 300 s; timing out first makes the page report a failure
// for an action that still completes server-side. tailscale serve may impose its
// own limit (not verified).
const rgResponseHeaderTimeout = 10 * time.Minute

// newRGProxy reverse-proxies to rg-api, the host daemon that fronts the Rogues
// Gallery GPUs. The request path passes through unchanged, so /api/rg/overview
// here is /api/rg/overview there. Unreachable backends answer 502
// rg_api_unreachable, which the RG GPUs page renders as its error state.
func newRGProxy(target string, logger *logmon.Monitor) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", rgAPIURLEnv, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s must be an http(s) URL with a host, got %q", rgAPIURLEnv, target)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = rgResponseHeaderTimeout

	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Host = r.Out.URL.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A cancelled request is not an rg-api failure, so keep it out of
			// the warning stream, as the peer proxy does.
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				logger.Debugf("rg-api proxy: request cancelled: %v", err)
			} else {
				logger.Warnf("rg-api proxy error: %v", err)
			}
			if swaputil.MarkClientClosed(w, r) || swaputil.ResponseStarted(w) {
				return
			}
			writeRGError(w, http.StatusBadGateway, "rg_api_unreachable", err.Error())
		},
	}, nil
}

// writeRGError writes the {"error", "detail"} shape the RG page reads, rather
// than swaputil's OpenAI-style envelope, because rg-api uses the same shape.
func writeRGError(w http.ResponseWriter, status int, code, detail string) {
	body := map[string]string{"error": code}
	if detail != "" {
		body["detail"] = detail
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// handleRG proxies /api/rg/* to rg-api, or answers 404 rg_api_not_configured
// when LLAMA_SWAP_RG_API_URL is unset.
func (s *Server) handleRG(w http.ResponseWriter, r *http.Request) {
	if s.rgProxy == nil {
		writeRGError(w, http.StatusNotFound, "rg_api_not_configured", rgAPIURLEnv+" is not set")
		return
	}
	s.rgProxy.ServeHTTP(w, r)
}
