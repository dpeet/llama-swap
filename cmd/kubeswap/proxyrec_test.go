package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An interim 100 forwarded by the reverse proxy must not mark the response as
// started, or a later proxy error is swallowed and the client gets an empty 200.
func TestStatusRecorder_InformationalDoesNotStartResponse(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.WriteHeader(http.StatusContinue)
	if rec.wrote {
		t.Fatal("wrote = true after 100 Continue, want false")
	}
	rec.WriteHeader(http.StatusBadGateway)
	if !rec.wrote {
		t.Fatal("wrote = false after 502, want true")
	}

	upgrade := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	upgrade.WriteHeader(http.StatusSwitchingProtocols)
	if !upgrade.wrote {
		t.Error("wrote = false after 101, want true: 101 is a final status")
	}
}
