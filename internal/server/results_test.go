package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newResultsTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.resultsDir = dir
	return s
}

func getResults(s *Server, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func assertResultsError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d, body %s", w.Code, status, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, w.Body.String())
	}
	if body["error"] != code {
		t.Errorf("error = %q, want %q", body["error"], code)
	}
}

func TestServer_ResultsNotConfigured(t *testing.T) {
	w := getResults(newResultsTestServer(t, ""), "/api/results/catalog.json")
	assertResultsError(t, w, http.StatusNotFound, "results_not_configured")
}

func TestServer_ResultsFileMissing(t *testing.T) {
	w := getResults(newResultsTestServer(t, t.TempDir()), "/api/results/catalog.json")
	assertResultsError(t, w, http.StatusNotFound, "results_file_missing")
}

func TestServer_ResultsUnknownName(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("x"), 0o644)
	s := newResultsTestServer(t, dir)
	for _, p := range []string{
		"/api/results/secret.txt",
		"/api/results/..%2Fcatalog.json",
		"/api/results/%2e%2e%2fsecret.txt",
		"/api/results/../secret.txt",
	} {
		w := getResults(s, p)
		// A raw ".." is path-cleaned by the mux into a 307 redirect to a
		// route that is itself rejected, so it is never served either.
		if w.Code != http.StatusNotFound && w.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s: status = %d, want 404 or 307", p, w.Code)
		}
		if w.Body.String() == "x" {
			t.Errorf("%s: leaked file content", p)
		}
	}
	assertResultsError(t, getResults(s, "/api/results/secret.txt"), http.StatusNotFound, "results_unknown_file")
}

func TestServer_ResultsServesAllowlistedFiles(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{
		"catalog.json":       "application/json",
		"schema.json":        "application/json",
		"measurements.jsonl": "application/x-ndjson",
	}
	s := newResultsTestServer(t, dir)
	for name, ct := range want {
		body := `{"file":"` + name + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		w := getResults(s, "/api/results/"+name)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", name, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != ct {
			t.Errorf("%s: Content-Type = %q, want %q", name, got, ct)
		}
		etag := w.Header().Get("ETag")
		if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' {
			t.Errorf("%s: ETag %q is not quoted", name, etag)
		}
		if w.Body.String() != body {
			t.Errorf("%s: body = %q", name, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q", name, got)
		}
	}
}

func TestServer_ResultsIfNoneMatch304(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "measurements.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o644)
	s := newResultsTestServer(t, dir)

	etag := getResults(s, "/api/results/measurements.jsonl").Header().Get("ETag")
	w := getResults(s, "/api/results/measurements.jsonl", "If-None-Match", etag)
	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w.Code)
	}

	// A changed file gets a new ETag, so the stale one no longer matches.
	os.WriteFile(path, []byte("{}\n{}\n"), 0o644)
	w = getResults(s, "/api/results/measurements.jsonl", "If-None-Match", etag)
	if w.Code != http.StatusOK {
		t.Errorf("after edit: status = %d, want 200", w.Code)
	}
}
