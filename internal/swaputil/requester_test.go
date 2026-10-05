package swaputil

import (
	"net/http/httptest"
	"testing"
)

func TestRequesterFrom(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		wantClient string
		wantIP     string
	}{
		{
			name:       "tailscale user device",
			remoteAddr: "127.0.0.1:51000",
			headers:    map[string]string{"Tailscale-User-Login": "dpeet@github", "X-Forwarded-For": "100.104.58.14"},
			wantClient: "ts:dpeet@github",
			wantIP:     "100.104.58.14",
		},
		{
			name:       "tagged device sends no login",
			remoteAddr: "127.0.0.1:51000",
			headers:    map[string]string{"X-Forwarded-For": "100.64.0.9, 10.0.0.1"},
			wantClient: "xff:100.64.0.9",
			wantIP:     "100.64.0.9",
		},
		{
			name:       "x-real-ip fallback",
			remoteAddr: "127.0.0.1:51000",
			headers:    map[string]string{"X-Real-IP": " 100.64.0.7 "},
			wantClient: "xff:100.64.0.7",
			wantIP:     "100.64.0.7",
		},
		{
			name:       "direct loopback caller",
			remoteAddr: "127.0.0.1:51000",
			wantClient: "ip:127.0.0.1",
			wantIP:     "127.0.0.1",
		},
		{
			name: "internal request has no peer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r.RemoteAddr = tt.remoteAddr
			r.Header.Set("User-Agent", "opencode/1.18.33")
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			got := RequesterFrom(r)
			if got.Client != tt.wantClient || got.IP != tt.wantIP {
				t.Errorf("Client=%q IP=%q, want %q %q", got.Client, got.IP, tt.wantClient, tt.wantIP)
			}
			if got.Method != "POST" || got.Path != "/v1/chat/completions" || got.UserAgent != "opencode/1.18.33" {
				t.Errorf("Method=%q Path=%q UserAgent=%q", got.Method, got.Path, got.UserAgent)
			}
		})
	}
}

func TestRequesterFrom_PathIsOriginalNotRewritten(t *testing.T) {
	r := httptest.NewRequest("GET", "/upstream/parakeet-asr/health?key=secret", nil)
	r.URL.Path = "/health" // what the /upstream handler leaves for the router

	if got := RequesterFrom(r).Path; got != "/upstream/parakeet-asr/health" {
		t.Errorf("Path=%q, want the client's path without the query", got)
	}
}
