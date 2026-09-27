package app_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"helm-github-releases-proxy/internal/app"
	"helm-github-releases-proxy/internal/config"
	githubclient "helm-github-releases-proxy/internal/github"
)

func TestAssembledChartReleaserDownloadsOnlyAdvertisedPackages(t *testing.T) {
	backend := &chartReleaserBackend{index: chartReleaserIndex(`release+build`, `demo%20chart.tgz`)}
	runtime := app.New(chartReleaserConfig(60), nil, discardLogger(), backend, fixedClock, context.Background())

	index := request(runtime.Handler, http.MethodGet, "http://index.example/index.yaml")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "charts/pages/release+build/nested/demo%20chart.tgz") || !strings.Contains(index.Body.String(), "charts/pages/package-in-branch/packages/nested/demo%20chart.tgz") {
		t.Fatalf("index = %d %q", index.Code, index.Body.String())
	}

	for _, test := range []struct {
		path, body, filename string
	}{
		{"charts/pages/release+build/nested/demo%20chart.tgz", "release bytes", "demo chart.tgz"},
		{"charts/pages/package-in-branch/packages/nested/demo%20chart.tgz", "branch bytes", "demo chart.tgz"},
	} {
		response := request(runtime.Handler, http.MethodGet, "http://different-host.example/"+test.path)
		if response.Code != http.StatusOK || response.Body.String() != test.body || response.Header().Get("Content-Type") != "application/gzip" || response.Header().Get("Content-Disposition") != `attachment; filename="`+test.filename+`"` {
			t.Errorf("GET %s = %d %q %v", test.path, response.Code, response.Body.String(), response.Header())
		}
	}

	for _, path := range []string{
		"charts/pages/release+build/nested/other.tgz",
		"charts/pages/package-in-branch/packages/nested/other.tgz",
		"charts/pages/release+build/other/../nested/demo%20chart.tgz",
		"charts/pages/release+build/nested/demo%2520chart.tgz",
	} {
		response := request(runtime.Handler, http.MethodGet, "http://different-host.example/"+path)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d %q, want 404", path, response.Code, response.Body.String())
		}
	}
	if backend.releaseCalls != 1 || backend.branchCalls != 1 {
		t.Fatalf("retrieval calls = release %d, branch %d", backend.releaseCalls, backend.branchCalls)
	}
}

func TestAssembledChartReleaserDownloadBeforePublicationDoesNotDiscoverOrWait(t *testing.T) {
	backend := &chartReleaserBackend{
		index:        chartReleaserIndex("release", "demo.tgz"),
		indexStarted: make(chan struct{}),
		indexRelease: make(chan struct{}),
	}
	runtime := app.New(chartReleaserConfig(60), nil, discardLogger(), backend, fixedClock, context.Background())

	before := request(runtime.Handler, http.MethodGet, "http://example.test/charts/pages/release/demo.tgz")
	if before.Code != http.StatusNotFound {
		t.Fatalf("before publication = %d, want 404", before.Code)
	}
	if backend.indexCalls != 0 || backend.releaseCalls != 0 || backend.branchCalls != 0 {
		t.Fatalf("download started discovery/retrieval: %#v", backend)
	}

	indexDone := make(chan struct{})
	go func() {
		defer close(indexDone)
		request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	}()
	<-backend.indexStarted

	downloadDone := make(chan int, 1)
	go func() {
		downloadDone <- request(runtime.Handler, http.MethodGet, "http://example.test/charts/pages/release/demo.tgz").Code
	}()
	select {
	case status := <-downloadDone:
		if status != http.StatusNotFound {
			t.Fatalf("during discovery = %d, want 404", status)
		}
	case <-time.After(time.Second):
		t.Fatal("download waited for chart-releaser discovery")
	}
	if backend.releaseCalls != 0 || backend.branchCalls != 0 {
		t.Fatalf("download during discovery retrieved a package: %#v", backend)
	}
	close(backend.indexRelease)
	<-indexDone
}

func TestAssembledChartReleaserPublicationMovesLookupTogether(t *testing.T) {
	backend := &chartReleaserBackend{index: chartReleaserIndex("old", "old.tgz")}
	runtime := app.New(chartReleaserConfig(0), nil, discardLogger(), backend, fixedClock, context.Background())

	first := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "old.tgz") {
		t.Fatalf("first index = %d %q", first.Code, first.Body.String())
	}
	backend.setIndex(chartReleaserIndex("new", "new.tgz"))
	backend.setIndexError(errors.New("GitHub unavailable"))
	failed := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if failed.Body.String() != first.Body.String() {
		t.Fatalf("failed refresh replaced publication:\n%s\nwant:\n%s", failed.Body.String(), first.Body.String())
	}
	assertChartReleaserDownloadStatus(t, runtime, "old/nested/old.tgz", http.StatusOK)
	assertChartReleaserDownloadStatus(t, runtime, "new/nested/new.tgz", http.StatusNotFound)

	backend.setIndexError(nil)
	replaced := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if replaced.Code != http.StatusOK || strings.Contains(replaced.Body.String(), "old.tgz") || !strings.Contains(replaced.Body.String(), "new.tgz") {
		t.Fatalf("replacement index = %d %q", replaced.Code, replaced.Body.String())
	}
	assertChartReleaserDownloadStatus(t, runtime, "old/nested/old.tgz", http.StatusNotFound)
	assertChartReleaserDownloadStatus(t, runtime, "new/nested/new.tgz", http.StatusOK)
}

type chartReleaserBackend struct {
	mu                         sync.Mutex
	index                      string
	indexErr                   error
	indexCalls, branchCalls    int
	releaseCalls               int
	indexStarted, indexRelease chan struct{}
	indexOnce                  sync.Once
}

func (backend *chartReleaserBackend) FetchBranchFile(_ context.Context, _, _, _, path, _ string) (io.ReadCloser, error) {
	backend.mu.Lock()
	backend.indexCalls++
	index, indexErr := backend.index, backend.indexErr
	started, release := backend.indexStarted, backend.indexRelease
	backend.mu.Unlock()
	if path != "index.yaml" {
		backend.mu.Lock()
		backend.branchCalls++
		backend.mu.Unlock()
		return io.NopCloser(strings.NewReader("branch bytes")), nil
	}
	if started != nil {
		backend.indexOnce.Do(func() { close(started) })
	}
	if release != nil {
		<-release
	}
	if indexErr != nil {
		return nil, indexErr
	}
	return io.NopCloser(strings.NewReader(index)), nil
}

func (backend *chartReleaserBackend) DownloadRelease(context.Context, string, string, string, string, string) (io.ReadCloser, error) {
	backend.mu.Lock()
	backend.releaseCalls++
	backend.mu.Unlock()
	return io.NopCloser(strings.NewReader("release bytes")), nil
}

func (backend *chartReleaserBackend) ListReleases(context.Context, string, string, string) ([]githubclient.Release, error) {
	return nil, errors.New("unexpected release listing")
}

func (backend *chartReleaserBackend) DownloadAsset(context.Context, string, string, int64, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected asset download")
}

func (backend *chartReleaserBackend) setIndex(index string) {
	backend.mu.Lock()
	backend.index = index
	backend.mu.Unlock()
}

func (backend *chartReleaserBackend) setIndexError(err error) {
	backend.mu.Lock()
	backend.indexErr = err
	backend.mu.Unlock()
}

func chartReleaserIndex(tag, filename string) string {
	return `entries:
  demo:
    - version: 1.0.0
      urls:
        - https://github.com/acme/charts/releases/download/` + tag + `/nested/` + filename + `
        - packages/nested/` + filename + `
`
}

func chartReleaserConfig(ttl int) config.Config {
	return config.Config{CacheTTLSeconds: ttl, Sources: []config.SourceConfig{{
		Name: "pages", Kind: config.ChartReleaserKind, Owner: "acme", Repo: "charts", Branch: "custom", GitHubToken: "secret",
	}}}
}

func assertChartReleaserDownloadStatus(t *testing.T, runtime *app.Runtime, suffix string, want int) {
	t.Helper()
	response := request(runtime.Handler, http.MethodGet, "http://example.test/charts/pages/"+suffix)
	if response.Code != want {
		t.Fatalf("GET %s = %d %q, want %d", suffix, response.Code, response.Body.String(), want)
	}
}
