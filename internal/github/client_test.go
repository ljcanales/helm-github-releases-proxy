package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"helm-github-releases-proxy/internal/logging"
	"log/slog"

	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestClientUsesOptionalTokenForReleaseAPIAndAssetDownloads(t *testing.T) {
	var requests []*http.Request
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request)
		body := `[{"published_at":"2026-08-30T12:00:00Z","assets":[{"id":7,"name":"demo-1.0.0.tgz","url":"https://api.github.test/assets/7"}]}]`
		if strings.HasSuffix(request.URL.Path, "/releases/assets/7") {
			body = "chart bytes"
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	client := NewClient(&http.Client{Transport: transport})

	releases, err := client.ListReleases(context.Background(), "acme", "charts", "secret")
	if err != nil || len(releases) != 1 || releases[0].Assets[0].ID != 7 {
		t.Fatalf("releases = %#v, error = %v", releases, err)
	}
	asset, err := client.DownloadAsset(context.Background(), "acme", "charts", 7, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer asset.Close()
	if data, _ := io.ReadAll(asset); string(data) != "chart bytes" {
		t.Fatalf("asset body = %q", data)
	}
	if len(requests) != 2 || requests[0].Header.Get("Authorization") != "Bearer secret" || requests[1].Header.Get("Accept") != "application/octet-stream" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestClientCanCallPublicRepositoryWithoutToken(t *testing.T) {
	client := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("[]")), Header: make(http.Header)}, nil
	})})
	if _, err := client.ListReleases(context.Background(), "acme", "public", ""); err != nil {
		t.Fatal(err)
	}
}

func githubResponse(status int, body string, header http.Header) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header}
}

func TestClientOperationEvents(t *testing.T) {
	for _, test := range []struct {
		name       string
		asset      func(*http.Request) (*http.Response, error)
		wantStatus int
		wantLevel  string
		wantFinal  string
	}{
		{name: "success", asset: func(*http.Request) (*http.Response, error) {
			return githubResponse(http.StatusOK, "chart bytes", make(http.Header)), nil
		}, wantStatus: http.StatusOK, wantLevel: "INFO"},
		{name: "redirect", asset: func(request *http.Request) (*http.Response, error) {
			if request.URL.Host == "api.github.com" {
				return githubResponse(http.StatusFound, "", http.Header{"Location": {"https://private.example/download?signature=signed-secret#fragment"}}), nil
			}
			return githubResponse(http.StatusOK, "chart bytes", make(http.Header)), nil
		}, wantStatus: http.StatusOK, wantLevel: "INFO", wantFinal: "private.example"},
		{name: "upstream error", asset: func(*http.Request) (*http.Response, error) {
			return githubResponse(http.StatusBadGateway, "bad gateway", make(http.Header)), nil
		}, wantStatus: http.StatusBadGateway, wantLevel: "WARN"},
		{name: "transport error", asset: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("Get https://private.example/download?signature=signed-secret: token secret connection failed")
		}, wantLevel: "WARN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			calls := 0
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return test.asset(request)
			})
			client := NewClient(&http.Client{Transport: transport}, logger)
			ctx := WithSource(logging.WithTraceID(context.Background(), "trace"), "releases")
			body, err := client.DownloadAsset(ctx, "acme", "charts", 7, "secret")
			if test.wantLevel == "INFO" {
				if err != nil {
					t.Fatal(err)
				}
				defer body.Close()
				data, readErr := io.ReadAll(body)
				if readErr != nil || string(data) != "chart bytes" {
					t.Fatalf("body = %q, error = %v", data, readErr)
				}
			} else if err == nil {
				t.Fatal("expected download error")
			}
			events := decodeClientEvents(t, output.Bytes())
			if len(events) != 1 || events[0]["msg"] != "github.request.completed" {
				t.Fatalf("events = %#v", events)
			}

			operation := events[0]
			if operation["level"] != test.wantLevel || operation["source"] != "releases" || operation["operation"] != "download_asset" || operation["method"] != http.MethodGet || operation["url"] != "https://api.github.com/repos/acme/charts/releases/assets/7" || operation["trace_id"] != "trace" {
				t.Fatalf("operation = %#v", operation)
			}
			if _, ok := operation["duration_ms"].(float64); !ok {
				t.Fatalf("duration missing: %#v", operation)
			}
			if test.wantStatus != 0 && operation["status"] != float64(test.wantStatus) {
				t.Fatalf("status = %#v", operation)
			}
			if test.wantStatus == 0 {
				if _, present := operation["status"]; present || !strings.Contains(operation["error"].(string), "<url-redacted>") {
					t.Fatalf("transport event = %#v", operation)
				}
			}
			if test.wantFinal != "" && (operation["final_scheme"] != "https" || operation["final_host"] != test.wantFinal || calls != 2) {
				t.Fatalf("redirect event = %#v, calls = %d", operation, calls)
			}
			if strings.Contains(output.String(), "signed-secret") || strings.Contains(output.String(), "token secret") || strings.Contains(output.String(), "/download") || strings.Contains(output.String(), "fragment") || strings.Contains(output.String(), "Location") {
				t.Fatalf("private redirect leaked: %s", output.String())
			}
		})
	}
}

func TestClientMalformedRedirectDiagnostics(t *testing.T) {
	for _, location := range []string{
		"/private-path%zz?signature=private-signature",
		"//private.example/private-path%zz?signature=private-signature",
		"https://private.example/private-path%zz?signature=private-signature",
	} {
		t.Run(location, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return githubResponse(302, "", http.Header{"Location": {location}}), nil
			})
			client := NewClient(&http.Client{Transport: transport}, logger)
			_, err := client.DownloadAsset(context.Background(), "acme", "charts", 7, "secret")
			if err == nil || !strings.Contains(err.Error(), location) {
				t.Fatalf("public error = %v", err)
			}
			events := decodeClientEvents(t, output.Bytes())
			if len(events) != 1 || events[0]["msg"] != "github.request.completed" || events[0]["level"] != "WARN" {
				t.Fatalf("events = %#v", events)
			}
			diagnostic, _ := events[0]["error"].(string)
			if !strings.Contains(diagnostic, "failed to parse Location header") || !strings.Contains(diagnostic, "<url-redacted>") {
				t.Fatalf("diagnostic = %q", diagnostic)
			}

			if strings.Contains(output.String(), "private-path") || strings.Contains(output.String(), "private-signature") {
				t.Fatalf("redirect leaked: %s", output.String())
			}
		})
	}
}

func TestClientProtectsTokenWithoutChangingPublicError(t *testing.T) {
	for _, operation := range []string{"discovery", "download"} {
		t.Run(operation, func(t *testing.T) {
			var output bytes.Buffer
			failure := errors.New("transport rejected private-token")
			client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, failure })}, slog.New(slog.NewJSONHandler(&output, nil)))
			var err error
			if operation == "discovery" {
				_, err = client.ListReleases(context.Background(), "acme", "charts", "private-token")
			} else {
				_, err = client.DownloadAsset(context.Background(), "acme", "charts", 7, "private-token")
			}
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("public error changed: %v", err)
			}
			if strings.Contains(output.String(), "private-token") || !strings.Contains(output.String(), "token-redacted") {
				t.Fatalf("unsafe log: %s", output.String())
			}
			if diagnostic := logging.RedactError(err); strings.Contains(diagnostic, "private-token") || !strings.Contains(diagnostic, "token-redacted") {
				t.Fatalf("unsafe propagated diagnostic: %s", diagnostic)
			}
		})
	}
}

func decodeClientEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err == io.EOF {
			return events
		} else if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}
