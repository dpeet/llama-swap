package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// newRGTestServer wires a Server whose rg-api proxy points at target.
func newRGTestServer(t *testing.T, target string) *Server {
	s, _ := newRGTestServerWithLog(t, target)
	return s
}

// newRGTestServerWithLog also returns the buffer the proxy logger writes to.
func newRGTestServerWithLog(t *testing.T, target string) (*Server, *bytes.Buffer) {
	t.Helper()
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	var buf bytes.Buffer
	proxy, err := newRGProxy(target, logmon.NewWriter(&buf))
	if err != nil {
		t.Fatalf("newRGProxy: %v", err)
	}
	s.rgProxy = proxy
	return s, &buf
}

func TestRGProxy_GetPathAndQueryPassThrough(t *testing.T) {
	var gotPath, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newRGTestServer(t, backend.URL)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/rg/overview?fresh=1&x=a%20b", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if gotPath != "/api/rg/overview" || gotQuery != "fresh=1&x=a%20b" {
		t.Errorf("backend saw path %q query %q", gotPath, gotQuery)
	}
	if w.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestRGProxy_PostBodyOriginAndContentTypePassThrough(t *testing.T) {
	var gotBody, gotOrigin, gotContentType, gotMethod string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotOrigin, gotContentType, gotMethod = string(b), r.Header.Get("Origin"), r.Header.Get("Content-Type"), r.Method
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"placed"}`))
	}))
	defer backend.Close()

	s := newRGTestServer(t, backend.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/rg/grab", strings.NewReader(`{"node":"best"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://rsch-ipat-d06.tailcfe95.ts.net")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want the backend's 202", w.Code)
	}
	if gotMethod != http.MethodPost || gotBody != `{"node":"best"}` {
		t.Errorf("backend saw %s body %q", gotMethod, gotBody)
	}
	if gotOrigin != "https://rsch-ipat-d06.tailcfe95.ts.net" || gotContentType != "application/json" {
		t.Errorf("backend saw Origin %q Content-Type %q", gotOrigin, gotContentType)
	}
}

func TestRGProxy_ForwardsRequesterIdentity(t *testing.T) {
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(rgClientHeader)
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()
	s := newRGTestServer(t, backend.URL)

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"tailscale login", map[string]string{"Tailscale-User-Login": "dpeet@github"}, "ts:dpeet@github"},
		// httptest.NewRequest's peer is 192.0.2.1:1234
		{"peer address", nil, "ip:192.0.2.1"},
		{"caller-supplied header is replaced", map[string]string{rgClientHeader: "ts:someone@else"}, "ip:192.0.2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/rg/overview", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			s.ServeHTTP(httptest.NewRecorder(), req)
			if got != tc.want {
				t.Errorf("rg-api saw %s %q, want %q", rgClientHeader, got, tc.want)
			}
		})
	}
}

func TestRGProxy_BackendClosedReturns502JSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := backend.URL
	backend.Close()

	s, logBuf := newRGTestServerWithLog(t, url)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/rg/overview", nil))

	if !strings.Contains(logBuf.String(), "rg-api proxy error") {
		t.Errorf("proxy error was not logged, log = %q", logBuf.String())
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	if got["error"] != "rg_api_unreachable" || got["detail"] == "" {
		t.Errorf("body = %v", got)
	}
}

func TestRGProxy_UnconfiguredReturns404JSON(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/rg/overview", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	if got["error"] != "rg_api_not_configured" {
		t.Errorf("body = %v", got)
	}
}

func TestRGProxy_RejectsUnusableTarget(t *testing.T) {
	for _, target := range []string{"127.0.0.1:8120", "ftp://x", "http://", "http://a b"} {
		if _, err := newRGProxy(target, logmon.NewWriter(io.Discard)); err == nil {
			t.Errorf("newRGProxy(%q) succeeded, want an error", target)
		}
	}
}

func TestRGProxy_HeaderTimeoutCoversRGAPIBounds(t *testing.T) {
	h, err := newRGProxy("http://127.0.0.1:1", logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatalf("newRGProxy: %v", err)
	}
	got := h.(*httputil.ReverseProxy).Transport.(*http.Transport).ResponseHeaderTimeout
	// rg-api's slowest answer is a release: rg.sh down (300 s) + rg-hold release (180 s).
	if got < 480*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want >= 480s", got)
	}
}
