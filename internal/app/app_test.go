package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"helm-github-releases-proxy/internal/app"
	"helm-github-releases-proxy/internal/chart"
	"helm-github-releases-proxy/internal/config"
	"helm-github-releases-proxy/internal/repository"
)

func TestAssembledPublishedSourceRequiresExactAdvertisedPath(t *testing.T) {
	opener := &testOpener{}
	path := "charts/assembled/nested/release%2Bbuild/widget-1.0.0.tgz"
	runtime := app.New(config.Config{CacheTTLSeconds: 60}, nil, discardLogger(), nil, fixedClock, context.Background(), app.WithSource(repository.Source{
		Config: repository.SourceConfig{Name: "assembled", Kind: "test"},
		Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
			return testContribution(path), nil
		}),
		Packages: opener,
	}))

	index := request(runtime.Handler, http.MethodGet, "http://index-host.example/index.yaml")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), path) {
		t.Fatalf("index response = %d %q", index.Code, index.Body.String())
	}

	advertised := request(runtime.Handler, http.MethodGet, "http://different-host.example/"+path)
	if advertised.Code != http.StatusOK || advertised.Body.String() != "package bytes" {
		t.Fatalf("advertised response = %d %q", advertised.Code, advertised.Body.String())
	}
	if got := advertised.Header().Get("Content-Disposition"); got != `attachment; filename="widget-1.0.0.tgz"` {
		t.Fatalf("Content-Disposition = %q", got)
	}

	for _, alternate := range []string{
		"charts/assembled/nested/release+build/widget-1.0.0.tgz",
		"charts/assembled/nested/release%252Bbuild/widget-1.0.0.tgz",
		"charts/assembled/nested/other/../release%2Bbuild/widget-1.0.0.tgz",
	} {
		response := request(runtime.Handler, http.MethodGet, "http://different-host.example/"+alternate)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %q status = %d, want 404", alternate, response.Code)
		}
	}
	if opener.Calls() != 1 {
		t.Fatalf("source opener calls = %d, want 1", opener.Calls())
	}
}

func TestAssembledDownloadDuringInitialDiscoveryReturnsImmediately(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	opener := &testOpener{}
	runtime := app.New(config.Config{CacheTTLSeconds: 60}, nil, discardLogger(), nil, fixedClock, context.Background(), app.WithSource(repository.Source{
		Config: repository.SourceConfig{Name: "assembled", Kind: "test"},
		Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
			close(started)
			<-release
			return testContribution("charts/assembled/widget-1.0.0.tgz"), nil
		}),
		Packages: opener,
	}))

	indexDone := make(chan struct{})
	go func() {
		defer close(indexDone)
		request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	}()
	<-started

	downloadDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		downloadDone <- request(runtime.Handler, http.MethodGet, "http://example.test/charts/assembled/widget-1.0.0.tgz")
	}()
	select {
	case response := <-downloadDone:
		if response.Code != http.StatusNotFound {
			t.Fatalf("download status = %d, want 404", response.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("download waited for initial discovery")
	}
	if opener.Calls() != 0 {
		t.Fatalf("source opener calls = %d, want 0", opener.Calls())
	}
	close(release)
	<-indexDone
}

type discoveryFunc func(context.Context) (repository.Contribution, error)

func (function discoveryFunc) Discover(ctx context.Context) (repository.Contribution, error) {
	return function(ctx)
}

type testOpener struct {
	mu    sync.Mutex
	calls int
}

func (opener *testOpener) OpenPackage(context.Context, repository.PackageReference) (repository.ChartPackage, error) {
	opener.mu.Lock()
	opener.calls++
	opener.mu.Unlock()
	return repository.ChartPackage{Body: io.NopCloser(strings.NewReader("package bytes")), Filename: "widget-1.0.0.tgz"}, nil
}

func (opener *testOpener) Calls() int {
	opener.mu.Lock()
	defer opener.mu.Unlock()
	return opener.calls
}

func testContribution(path string) repository.Contribution {
	return repository.Contribution{ChartVersions: []repository.DiscoveredChartVersion{{
		Version:  chart.Version{APIVersion: "v2", Name: "widget", Version: "1.0.0"},
		Packages: []repository.DiscoveredPackage{{AdvertisedPath: path, Reference: repository.PackageReference{Source: "assembled", Key: "opaque"}}},
	}}, IndexedCount: 1}
}

func request(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, target, nil))
	return response
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fixedClock() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }
