package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"helm-github-releases-proxy/internal/httpapi"
	"helm-github-releases-proxy/internal/logging"
)

func TestRequestLoggingFinalStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		serve  func(http.ResponseWriter, *http.Request)
		status int
		panics bool
	}{
		{name: "implicit OK", serve: func(http.ResponseWriter, *http.Request) {}, status: 200},
		{name: "informational then final", serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); w.WriteHeader(204) }, status: 204},
		{name: "informational then body", serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); _, _ = w.Write([]byte("body")) }, status: 200},
		{name: "switching protocols", serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(101) }, status: 101},
		{name: "duplicate final", serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201); w.WriteHeader(202) }, status: 201},
		{name: "panic before final", serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); panic(http.ErrAbortHandler) }, panics: true},
		{name: "panic after final", serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(202); panic(http.ErrAbortHandler) }, status: 202, panics: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			handler := httpapi.LogRequests(http.HandlerFunc(test.serve), logging.New(slog.New(slog.NewJSONHandler(&output, nil))))
			func() {
				defer func() {
					recovered := recover()
					if test.panics && recovered != http.ErrAbortHandler || !test.panics && recovered != nil {
						t.Fatalf("panic = %v", recovered)
					}
				}()
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			}()
			if test.panics && strings.Contains(output.String(), http.ErrAbortHandler.Error()) {
				t.Fatalf("panic value leaked: %s", output.String())
			}
			decoder := json.NewDecoder(&output)
			var arrival, completion map[string]any
			if err := decoder.Decode(&arrival); err != nil {
				t.Fatal(err)
			}
			if err := decoder.Decode(&completion); err != nil {
				t.Fatal(err)
			}
			if completion["msg"] != "http.response" {
				t.Fatalf("completion = %#v", completion)
			}
			if test.status == 0 {
				if _, present := completion["status"]; present {
					t.Fatalf("uncommitted status: %#v", completion)
				}
			} else if completion["status"] != float64(test.status) {
				t.Fatalf("status = %v, want %d", completion["status"], test.status)
			}
		})
	}
}

func TestRequestLoggingFieldsAndCorrelation(t *testing.T) {
	var output bytes.Buffer
	var activeTrace string
	handler := httpapi.LogRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeTrace = logging.TraceID(r.Context())
		httpapi.RecordError(r.Context(), errors.New(`Get "https://private.example/signed-secret": failed`))
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("failure"))
	}), logging.New(logging.NewJSON(&output, slog.LevelInfo)))
	seen := map[string]bool{}
	for range 2 {
		output.Reset()
		request := httptest.NewRequest(http.MethodGet, "/charts/a%20b.tgz?token=query-secret", nil)
		request.Header.Set("Traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		decoder := json.NewDecoder(&output)
		for _, name := range []string{"http.request", "http.response"} {
			var event map[string]any
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event["msg"] != name || event["level"] != "INFO" || event["method"] != "GET" || event["path"] != "/charts/a b.tgz" || event["trace_id"] != activeTrace {
				t.Fatalf("event = %#v", event)
			}
			if _, present := event["route"]; present {
				t.Fatalf("unexpected route: %#v", event)
			}
			if name == "http.request" {
				if _, present := event["status"]; present {
					t.Fatalf("request has status: %#v", event)
				}
			} else {
				if event["status"] != float64(http.StatusBadGateway) || event["bytes_written"] != float64(response.Body.Len()) || event["error"] != `Get "<url-redacted>": failed` {
					t.Fatalf("response = %#v", event)
				}
				if _, ok := event["duration_ms"].(float64); !ok {
					t.Fatalf("missing duration: %#v", event)
				}
			}
		}
		var extra map[string]any
		if err := decoder.Decode(&extra); err != io.EOF {
			t.Fatalf("unexpected extra event: %#v, error = %v", extra, err)
		}
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(activeTrace) || activeTrace == strings.Repeat("a", 32) || seen[activeTrace] {
			t.Fatalf("trace ID = %q", activeTrace)
		}
		seen[activeTrace] = true
	}
}
