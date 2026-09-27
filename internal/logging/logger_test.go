package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"helm-github-releases-proxy/internal/logging"
)

func TestGenericLoggingStructuredErrorsAndContext(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.NewJSON(&output, slog.LevelInfo))
	ctx := logging.WithFields(logging.WithTraceID(context.Background(), "trace"), "job_id", "job")
	failure := errors.New(`Get "https://private.example/secret": failed`)
	details := map[string]any{"attempts": 2, "error": failure}
	logger.Debug(ctx, "hidden")
	logger.Info(ctx, "job.started", "name", "example")
	logger.Warn(ctx, "job.failed", "results", []any{details})
	logger.Error(ctx, "job.stopped", slog.Group("details", slog.Any("error", failure)))
	decoder := json.NewDecoder(&output)
	for _, expected := range []struct{ message, level string }{
		{"job.started", "INFO"}, {"job.failed", "WARN"}, {"job.stopped", "ERROR"},
	} {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["msg"] != expected.message || event["level"] != expected.level || event["trace_id"] != "trace" || event["job_id"] != "job" {
			t.Fatalf("event = %#v", event)
		}
		if expected.message == "job.failed" {
			result := event["results"].([]any)[0].(map[string]any)
			if result["attempts"] != float64(2) || result["error"] != `Get "<url-redacted>": failed` {
				t.Fatalf("result = %#v", result)
			}
		}
		if expected.message == "job.stopped" && event["details"].(map[string]any)["error"] != `Get "<url-redacted>": failed` {
			t.Fatalf("details = %#v", event)
		}
	}
	if details["error"] != failure {
		t.Fatal("logging mutated caller fields")
	}
}

func TestHTTPClientLoggingIsIndependentOfService(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		err    error
		level  string
	}{
		{name: "success", status: 201, level: "INFO"},
		{name: "http failure", status: 503, level: "WARN"},
		{name: "transport failure", err: errors.New("Post https://private.example/secret: failed"), level: "WARN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := logging.New(logging.NewJSON(&output, slog.LevelInfo))
			logger.HTTPClientCompleted(context.Background(), "http.client.completed", logging.HTTPClientResponse{
				Method: "POST", URL: "https://inventory.example/items", Status: test.status,
				Duration: 25 * time.Millisecond, Error: test.err,
			}, "service", "inventory")
			if strings.Contains(output.String(), "private.example") || strings.Contains(output.String(), "secret") {
				t.Fatalf("unsafe log: %s", output.String())
			}
			var event map[string]any
			if err := json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["msg"] != "http.client.completed" || event["level"] != test.level || event["method"] != "POST" || event["service"] != "inventory" || event["duration_ms"] != float64(25) {
				t.Fatalf("event = %#v", event)
			}
			if test.status == 0 {
				if _, present := event["status"]; present {
					t.Fatalf("invented status: %#v", event)
				}
			} else if event["status"] != float64(test.status) {
				t.Fatalf("status = %#v", event)
			}
		})
	}
}
