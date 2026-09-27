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

func TestAssembledGitHubReleasesDownloadsOnlyAdvertisedAssets(t *testing.T) {
	backend := &trackingReleaseBackend{
		releases:  []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: "demo-1.2.3.tgz"}}}},
		assetBody: "published asset bytes",
	}
	runtime := app.New(releaseConfig(60), nil, discardLogger(), backend, fixedClock, context.Background())

	index := request(runtime.Handler, http.MethodGet, "http://index.example/index.yaml")
	advertisedPath := "charts/releases/7/demo-1.2.3.tgz"
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), advertisedPath) {
		t.Fatalf("index response = %d %q", index.Code, index.Body.String())
	}
	download := request(runtime.Handler, http.MethodGet, "http://different-host.example/"+advertisedPath)
	if download.Code != http.StatusOK || download.Body.String() != "published asset bytes" {
		t.Fatalf("download response = %d %q", download.Code, download.Body.String())
	}
	if download.Header().Get("Content-Type") != "application/gzip" || download.Header().Get("Content-Disposition") != `attachment; filename="demo-1.2.3.tgz"` {
		t.Fatalf("download headers = %v", download.Header())
	}
	listCalls, downloads := backend.activity()
	if listCalls != 1 || len(downloads) != 1 || downloads[0] != (assetRequest{owner: "acme", repo: "charts", id: 7, token: "secret"}) {
		t.Fatalf("backend activity = list %d, downloads %#v", listCalls, downloads)
	}

	for _, path := range []string{
		"charts/releases/8/demo-1.2.3.tgz",
		"charts/releases/7/wrong-1.2.3.tgz",
		"charts/releases/07/demo-1.2.3.tgz",
		"charts/releases/+7/demo-1.2.3.tgz",
	} {
		response := request(runtime.Handler, http.MethodGet, "http://different-host.example/"+path)
		if response.Code != http.StatusNotFound || response.Body.String() != "chart not found\n" {
			t.Errorf("GET %s = %d %q, want 404", path, response.Code, response.Body.String())
		}
	}
	_, downloads = backend.activity()
	if len(downloads) != 1 {
		t.Fatalf("rejected paths caused downloads: %#v", downloads)
	}
}

func TestAssembledGitHubReleaseDownloadBeforePublicationDoesNotDiscoverOrWait(t *testing.T) {
	backend := &trackingReleaseBackend{
		releases:    []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: "demo-1.0.0.tgz"}}}},
		assetBody:   "asset bytes",
		listStarted: make(chan struct{}),
		listRelease: make(chan struct{}),
	}
	runtime := app.New(releaseConfig(60), nil, discardLogger(), backend, fixedClock, context.Background())

	before := request(runtime.Handler, http.MethodGet, "http://example.test/charts/releases/7/demo-1.0.0.tgz")
	if before.Code != http.StatusNotFound {
		t.Fatalf("download before discovery = %d, want 404", before.Code)
	}
	if listCalls, downloads := backend.activity(); listCalls != 0 || len(downloads) != 0 {
		t.Fatalf("download started backend activity: list %d, downloads %#v", listCalls, downloads)
	}

	indexDone := make(chan struct{})
	go func() {
		defer close(indexDone)
		request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	}()
	<-backend.listStarted
	downloadDone := make(chan int, 1)
	go func() {
		downloadDone <- request(runtime.Handler, http.MethodGet, "http://example.test/charts/releases/7/demo-1.0.0.tgz").Code
	}()
	select {
	case status := <-downloadDone:
		if status != http.StatusNotFound {
			t.Fatalf("download during discovery = %d, want 404", status)
		}
	case <-time.After(time.Second):
		t.Fatal("download waited for initial GitHub discovery")
	}
	if _, downloads := backend.activity(); len(downloads) != 0 {
		t.Fatalf("download during discovery retrieved an asset: %#v", downloads)
	}
	close(backend.listRelease)
	<-indexDone
}

func TestAssembledGitHubReleasePublicationMovesIndexAndAssetLookupTogether(t *testing.T) {
	backend := &trackingReleaseBackend{
		releases:  []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: "demo-1.0.0.tgz"}}}},
		assetBody: "asset bytes",
	}
	runtime := app.New(releaseConfig(0), nil, discardLogger(), backend, fixedClock, context.Background())

	first := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "charts/releases/7/demo-1.0.0.tgz") {
		t.Fatalf("first index = %d %q", first.Code, first.Body.String())
	}
	backend.setReleases([]githubclient.Release{{Assets: []githubclient.Asset{{ID: 8, Name: "demo-2.0.0.tgz"}}}})
	backend.setListError(errors.New("GitHub unavailable"))
	failed := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if failed.Body.String() != first.Body.String() {
		t.Fatalf("failed refresh replaced publication:\n%s\nwant:\n%s", failed.Body.String(), first.Body.String())
	}
	assertReleaseDownloadStatus(t, runtime, "7/demo-1.0.0.tgz", http.StatusOK)
	assertReleaseDownloadStatus(t, runtime, "8/demo-2.0.0.tgz", http.StatusNotFound)

	backend.setListError(nil)
	replaced := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if replaced.Code != http.StatusOK || strings.Contains(replaced.Body.String(), "/7/demo-1.0.0.tgz") || !strings.Contains(replaced.Body.String(), "charts/releases/8/demo-2.0.0.tgz") {
		t.Fatalf("replacement index = %d %q", replaced.Code, replaced.Body.String())
	}
	assertReleaseDownloadStatus(t, runtime, "7/demo-1.0.0.tgz", http.StatusNotFound)
	assertReleaseDownloadStatus(t, runtime, "8/demo-2.0.0.tgz", http.StatusOK)
}

type assetRequest struct {
	owner, repo, token string
	id                 int64
}

type trackingReleaseBackend struct {
	mu                       sync.Mutex
	releases                 []githubclient.Release
	listErr                  error
	assetBody                string
	listCalls                int
	downloads                []assetRequest
	listStarted, listRelease chan struct{}
	startOnce                sync.Once
}

func (backend *trackingReleaseBackend) ListReleases(_ context.Context, owner, repo, token string) ([]githubclient.Release, error) {
	backend.mu.Lock()
	backend.listCalls++
	releases, err := backend.releases, backend.listErr
	started, release := backend.listStarted, backend.listRelease
	backend.mu.Unlock()
	if started != nil {
		backend.startOnce.Do(func() { close(started) })
	}
	if release != nil {
		<-release
	}
	return releases, err
}

func (backend *trackingReleaseBackend) DownloadAsset(_ context.Context, owner, repo string, id int64, token string) (io.ReadCloser, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.downloads = append(backend.downloads, assetRequest{owner: owner, repo: repo, id: id, token: token})
	return io.NopCloser(strings.NewReader(backend.assetBody)), nil
}

func (backend *trackingReleaseBackend) FetchBranchFile(context.Context, string, string, string, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected branch request")
}

func (backend *trackingReleaseBackend) DownloadRelease(context.Context, string, string, string, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected release download")
}

func (backend *trackingReleaseBackend) activity() (int, []assetRequest) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.listCalls, append([]assetRequest(nil), backend.downloads...)
}

func (backend *trackingReleaseBackend) setReleases(releases []githubclient.Release) {
	backend.mu.Lock()
	backend.releases = releases
	backend.mu.Unlock()
}

func (backend *trackingReleaseBackend) setListError(err error) {
	backend.mu.Lock()
	backend.listErr = err
	backend.mu.Unlock()
}

func releaseConfig(ttl int) config.Config {
	return config.Config{CacheTTLSeconds: ttl, Sources: []config.SourceConfig{{
		Name: "releases", Kind: config.GitHubReleasesKind, Owner: "acme", Repo: "charts", GitHubToken: "secret",
	}}}
}

func assertReleaseDownloadStatus(t *testing.T, runtime *app.Runtime, suffix string, want int) {
	t.Helper()
	response := request(runtime.Handler, http.MethodGet, "http://example.test/charts/releases/"+suffix)
	if response.Code != want {
		t.Fatalf("GET %s = %d %q, want %d", suffix, response.Code, response.Body.String(), want)
	}
}
