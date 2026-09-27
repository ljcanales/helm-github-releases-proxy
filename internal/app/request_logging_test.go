package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"helm-github-releases-proxy/internal/app"
	"helm-github-releases-proxy/internal/config"
	"helm-github-releases-proxy/internal/repository"
)

func TestAssembledRequestEvents(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	runtime := app.New(config.Config{CacheTTLSeconds: 60}, nil, logger, nil, fixedClock, context.Background(), app.WithSource(repository.Source{
		Config: repository.SourceConfig{Name: "assembled", Kind: "test"},
		Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
			return testContribution("charts/assembled/widget-1.0.0.tgz"), nil
		}),
		Packages: &testOpener{},
	}))

	cases := []struct {
		name, target, path string
		status             int
	}{
		{"liveness", "/livez?token=secret", "/livez", http.StatusOK},
		{"ready", "/readyz", "/readyz", http.StatusOK},
		{"status", "/status", "/status", http.StatusOK},
		{"index", "/index.yaml", "/index.yaml", http.StatusOK},
		{"download", "/charts/assembled/widget-1.0.0.tgz", "/charts/assembled/widget-1.0.0.tgz", http.StatusOK},
		{"missing chart", "/charts/assembled/a%20b.tgz", "/charts/assembled/a b.tgz", http.StatusNotFound},
		{"missing route", "/absent", "/absent", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output.Reset()
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://example.test"+tc.target, nil)
			request.Header.Set("Traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
			runtime.Handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			events := decodeEvents(t, output.Bytes())
			if tc.name == "index" {
				if len(events) != 3 || events[1]["msg"] != "repository.refresh.completed" {
					t.Fatalf("index events = %#v", events)
				}
				events = []map[string]any{events[0], events[2]}
			}
			if len(events) != 2 {
				t.Fatalf("events = %#v", events)
			}
			for i, name := range []string{"http.request", "http.response"} {
				if events[i]["msg"] != name || events[i]["level"] != "INFO" || events[i]["path"] != tc.path {
					t.Fatalf("event = %#v", events[i])
				}
			}
			if events[1]["status"] != float64(tc.status) {
				t.Fatalf("response event = %#v", events[1])
			}
		})
	}
}

func TestAssembledRequestEventsPreservePublicErrors(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	runtime := app.New(config.Config{CacheTTLSeconds: 60}, nil, logger, nil, fixedClock, context.Background(), app.WithSource(repository.Source{
		Config: repository.SourceConfig{Name: "assembled", Kind: "test"},
		Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
			return testContribution("charts/assembled/widget-1.0.0.tgz"), nil
		}),
		Packages: urlErrorOpener{},
	}))
	request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	output.Reset()
	response := request(runtime.Handler, http.MethodGet, "http://example.test/charts/assembled/widget-1.0.0.tgz")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	events := decodeEvents(t, output.Bytes())
	if len(events) != 2 || events[1]["status"] != float64(http.StatusBadGateway) || events[1]["error"] != `Get "<url-redacted>": upstream failed` {
		t.Fatalf("events = %#v", events)
	}
	if strings.Contains(output.String(), "signed-secret") || !strings.Contains(response.Body.String(), "signed-secret") {
		t.Fatalf("logging changed HTTP body or exposed URL: log=%s body=%s", output.String(), response.Body.String())
	}
}

func TestAssembledRequestEventsForNotReady(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	runtime := app.New(config.Config{}, errors.New("invalid configuration"), logger, nil, fixedClock, context.Background())
	response := request(runtime.Handler, http.MethodGet, "http://example.test/readyz")
	events := decodeEvents(t, output.Bytes())
	if response.Code != http.StatusServiceUnavailable || len(events) != 2 || events[1]["status"] != float64(http.StatusServiceUnavailable) || events[1]["level"] != "INFO" {
		t.Fatalf("response = %d, events = %#v", response.Code, events)
	}
}

func TestAssembledRequestEventsForInterruptedChart(t *testing.T) {
	for _, test := range []struct {
		name             string
		opener           repository.PackageOpener
		accept           int
		body, diagnostic string
	}{
		{name: "upstream read", opener: interruptedOpener{}, body: "partial", diagnostic: `Get "<url-redacted>": interrupted`},
		{name: "downstream write", opener: &testOpener{}, accept: 7, body: "package", diagnostic: "write <url-redacted> interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			runtime := app.New(config.Config{CacheTTLSeconds: 60}, nil, logger, nil, fixedClock, context.Background(), app.WithSource(repository.Source{
				Config: repository.SourceConfig{Name: "assembled", Kind: "test"},
				Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
					return testContribution("charts/assembled/widget-1.0.0.tgz"), nil
				}),
				Packages: test.opener,
			}))
			request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
			output.Reset()
			response := httptest.NewRecorder()
			var writer http.ResponseWriter = response
			if test.accept != 0 {
				writer = &interruptingResponseWriter{ResponseRecorder: response, accept: test.accept}
			}
			runtime.Handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "http://example.test/charts/assembled/widget-1.0.0.tgz", nil))
			events := decodeEvents(t, output.Bytes())
			if response.Code != http.StatusOK || response.Body.String() != test.body || len(events) != 2 {
				t.Fatalf("response = %d %q, events = %#v", response.Code, response.Body.String(), events)
			}
			event := events[1]
			if event["msg"] != "http.response" || event["status"] != float64(http.StatusOK) || event["bytes_written"] != float64(len(test.body)) || event["error"] != test.diagnostic {
				t.Fatalf("response event = %#v", event)
			}
			if strings.Contains(output.String(), "signed-secret") {
				t.Fatalf("stream URL leaked: %s", output.String())
			}
		})
	}
}

type interruptingResponseWriter struct {
	*httptest.ResponseRecorder
	accept int
}

func (writer *interruptingResponseWriter) Write(body []byte) (int, error) {
	n, _ := writer.ResponseRecorder.Write(body[:min(len(body), writer.accept)])
	return n, errors.New("write https://private.example/download?signature=signed-secret: interrupted")
}

type interruptedOpener struct{}

func (interruptedOpener) OpenPackage(context.Context, repository.PackageReference) (repository.ChartPackage, error) {
	return repository.ChartPackage{Body: io.NopCloser(io.MultiReader(strings.NewReader("partial"), interruptedReader{})), Filename: "widget-1.0.0.tgz"}, nil
}

type interruptedReader struct{}

func (interruptedReader) Read([]byte) (int, error) {
	return 0, errors.New(`Get "https://private.example/download?signature=signed-secret": interrupted`)
}

type urlErrorOpener struct{}

func (urlErrorOpener) OpenPackage(context.Context, repository.PackageReference) (repository.ChartPackage, error) {
	return repository.ChartPackage{}, errors.New(`Get "https://user:pass@private.example/download?signature=signed-secret": upstream failed`)
}

func decodeEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
