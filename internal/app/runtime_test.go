package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"helm-github-releases-proxy/internal/app"
	"helm-github-releases-proxy/internal/config"
	"helm-github-releases-proxy/internal/repository"
)

func TestRuntimeLivenessReadinessAndConfigurationOnlyStatus(t *testing.T) {
	valid := app.New(config.Config{CacheTTLSeconds: 60}, nil, discardLogger(), nil, fixedClock, context.Background())
	if response := request(valid.Handler, http.MethodGet, "http://example.test/livez"); response.Code != http.StatusOK || response.Body.String() != `{"status":"ok"}` {
		t.Fatalf("liveness = %d %q", response.Code, response.Body.String())
	}
	if response := request(valid.Handler, http.MethodGet, "http://example.test/readyz"); response.Code != http.StatusOK || response.Body.String() != `{"status":"ok"}` {
		t.Fatalf("valid readiness = %d %q", response.Code, response.Body.String())
	}
	status := request(valid.Handler, http.MethodGet, "http://example.test/status")
	if status.Code != http.StatusOK || !containsAll(status.Body.String(), `"status":"unknown"`, `"cache_present":false`, `"repositories":[]`) {
		t.Fatalf("initial status = %d %q", status.Code, status.Body.String())
	}

	invalid := app.New(config.Config{}, errors.New("invalid configuration"), discardLogger(), nil, fixedClock, context.Background())
	if response := request(invalid.Handler, http.MethodGet, "http://example.test/livez"); response.Code != http.StatusOK {
		t.Fatalf("invalid liveness = %d", response.Code)
	}
	if response := request(invalid.Handler, http.MethodGet, "http://example.test/readyz"); response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("invalid readiness = %d %q", response.Code, response.Body.String())
	}
}

func TestRuntimeStatusSerializesSourceStateAndSanitizesErrors(t *testing.T) {
	runtime := app.New(config.Config{CacheTTLSeconds: 60}, nil, discardLogger(), nil, fixedClock, context.Background(), app.WithSource(repository.Source{
		Config: repository.SourceConfig{Name: "local", Kind: repository.LocalSource, Owner: "ignored", Path: "charts"},
		Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
			return repository.Contribution{}, errors.New("open /private/charts: permission denied")
		}),
		Packages: failingOpener{},
	}))

	index := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if index.Code != http.StatusOK {
		t.Fatalf("partial index = %d %q", index.Code, index.Body.String())
	}
	var status struct {
		Status       string `json:"status"`
		LastAttempt  string `json:"last_attempt_at"`
		LastSuccess  string `json:"last_success_at"`
		CachePresent bool   `json:"cache_present"`
		Repositories []struct {
			Name         string `json:"name"`
			Type         string `json:"type"`
			Path         string `json:"path"`
			Status       string `json:"status"`
			IndexedCount int    `json:"indexed_count"`
			Error        string `json:"error"`
		} `json:"repositories"`
	}
	response := request(runtime.Handler, http.MethodGet, "http://example.test/status")
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "partial" || status.LastAttempt == "" || status.LastSuccess != "" || !status.CachePresent || len(status.Repositories) != 1 {
		t.Fatalf("status = %#v", status)
	}
	source := status.Repositories[0]
	if source.Name != "local" || source.Type != "local-directory" || source.Path != "charts" || source.Status != "failed" || source.IndexedCount != 0 || source.Error == "" || strings.Contains(source.Error, "private") || strings.Contains(source.Error, "/private/charts") {
		t.Fatalf("source status = %#v", source)
	}
}

func TestRuntimeMapsEligibleSourceErrorsToBadGateway(t *testing.T) {
	runtime := app.New(config.Config{CacheTTLSeconds: 60}, nil, discardLogger(), nil, fixedClock, context.Background(), app.WithSource(repository.Source{
		Config: repository.SourceConfig{Name: "assembled", Kind: "test"},
		Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
			return testContribution("charts/assembled/widget-1.0.0.tgz"), nil
		}),
		Packages: failingOpener{},
	}))
	index := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if index.Code != http.StatusOK {
		t.Fatalf("index = %d %q", index.Code, index.Body.String())
	}
	download := request(runtime.Handler, http.MethodGet, "http://example.test/charts/assembled/widget-1.0.0.tgz")
	if download.Code != http.StatusBadGateway || download.Body.String() != "upstream package failed\n" {
		t.Fatalf("download = %d %q", download.Code, download.Body.String())
	}
}

type failingOpener struct{}

func (failingOpener) OpenPackage(context.Context, repository.PackageReference) (repository.ChartPackage, error) {
	return repository.ChartPackage{}, errors.New("upstream package failed")
}

func containsAll(value string, wanted ...string) bool {
	for _, part := range wanted {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
