package server

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestServer_ExpectContinue_ErrorStatusSurvives drives a real client, the full
// middleware chain and an httputil.ReverseProxy against an upstream that sends
// 100 Continue and then rejects the request with 400 — the shape of a large
// multipart upload to /v1/audio/transcriptions that the upstream refuses. The
// proxy forwards the 100 through WriteHeader, and every wrapper used to latch
// it as the final status: the client received 200 with the 400's body, and the
// access log and activity store recorded 100 or 200.
func TestServer_ExpectContinue_ErrorStatusSurvives(t *testing.T) {
	const errBody = `{"error":{"message":"bad audio","code":400}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Answer before reading the body, as an upstream that validates
		// headers first does. The explicit 100 stands in for the interim
		// response such an upstream sends to an Expect: 100-continue request.
		w.WriteHeader(http.StatusContinue)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, errBody)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	local := newStubRouter([]string{"asr"}, "")
	local.serveHTTP = proxy.ServeHTTP
	s := newTestServer(local, newStubRouter(nil, ""))
	front := httptest.NewServer(s)
	defer front.Close()

	// A body over 1 MB, the size at which curl starts sending
	// Expect: 100-continue.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("model", "asr"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte{0}, 1536<<10)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/audio/transcriptions", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Expect", "100-continue")
	client := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: 5 * time.Second}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("client status = %d, want 400 (body %q)", resp.StatusCode, got)
	}
	if string(got) != errBody {
		t.Errorf("client body = %q, want %q", got, errBody)
	}

	// The access log line and the activity entry are written after the
	// handler returns, which can be just after the client has the response.
	wantLog := fmt.Sprintf("POST /v1/audio/transcriptions HTTP/1.1\" %d ", http.StatusBadRequest)
	deadline := time.Now().Add(5 * time.Second)
	for {
		logged := string(s.logs.HttpLogs.GetHistory())
		entries := metricsEntries(t, s.metrics)
		if strings.Contains(logged, wantLog) && len(entries) == 1 && entries[0].RespStatusCode == http.StatusBadRequest {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("access log missing %q:\n%s", wantLog, logged)
			for _, e := range entries {
				t.Errorf("activity entry status = %d, want 400", e.RespStatusCode)
			}
			if len(entries) != 1 {
				t.Errorf("activity entries = %d, want 1", len(entries))
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// codeLog is a ResponseWriter that records every WriteHeader call, because
// httptest.ResponseRecorder latches the first code, 1xx included.
type codeLog struct {
	header http.Header
	codes  []int
}

func (c *codeLog) Header() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}
func (c *codeLog) Write(b []byte) (int, error) { return len(b), nil }
func (c *codeLog) WriteHeader(code int)        { c.codes = append(c.codes, code) }

// TestServer_ResponseWrappers_PassInformationalThrough pins each wrapper
// individually: an interim 100 is forwarded without becoming the status, the
// following 400 is the one recorded, and 101 still counts as final.
func TestServer_ResponseWrappers_PassInformationalThrough(t *testing.T) {
	type wrapper struct {
		http.ResponseWriter
		status func() int
	}
	wrappers := map[string]func(http.ResponseWriter) wrapper{
		"statusRecorder": func(w http.ResponseWriter) wrapper {
			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			return wrapper{sr, func() int { return sr.status }}
		},
		"responseBodyCopier": func(w http.ResponseWriter) wrapper {
			bc := newBodyCopier(w)
			return wrapper{bc, bc.Status}
		},
		"inflightResponseWriter": func(w http.ResponseWriter) wrapper {
			iw := &inflightResponseWriter{ResponseWriter: w, tracker: newInflightTracker(), id: "x"}
			return wrapper{iw, nil}
		},
	}
	cases := []struct {
		name       string
		calls      []int
		wantCodes  []int
		wantStatus int
	}{
		{"100 then 400", []int{100, 400}, []int{100, 400}, 400},
		{"103 then 200", []int{103, 200}, []int{103, 200}, 200},
		{"101 is final", []int{101, 400}, []int{101}, 101},
		{"400 then 100 dropped", []int{400, 100}, []int{400}, 400},
	}
	for name, wrap := range wrappers {
		for _, c := range cases {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				under := &codeLog{}
				w := wrap(under)
				for _, code := range c.calls {
					w.WriteHeader(code)
				}
				if fmt.Sprint(under.codes) != fmt.Sprint(c.wantCodes) {
					t.Errorf("underlying WriteHeader calls = %v, want %v", under.codes, c.wantCodes)
				}
				if w.status != nil && w.status() != c.wantStatus {
					t.Errorf("recorded status = %d, want %d", w.status(), c.wantStatus)
				}
			})
		}
	}
}
