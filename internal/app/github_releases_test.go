package app_test

import (
	"bytes"
	"log/slog"

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

type githubTransportFunc func(*http.Request) (*http.Response, error)

func (f githubTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func githubResponse(status int, body string, header http.Header) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header}
}

func TestAssembledGitHubDiscoveryCorrelation(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	transport := githubTransportFunc(func(request *http.Request) (*http.Response, error) {
		return githubResponse(http.StatusOK, `[{"assets":[{"id":7,"name":"demo-1.0.0.tgz"},{"id":8,"name":"notes.txt"}]}]`, make(http.Header)), nil
	})
	runtime := app.New(releaseConfig(60), nil, logger, githubclient.NewClient(&http.Client{Transport: transport}, logger), fixedClock, context.Background())
	if response := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml"); response.Code != http.StatusOK {
		t.Fatalf("index = %d", response.Code)
	}
	events := decodeEvents(t, output.Bytes())
	var refreshes []map[string]any
	for _, event := range events {
		if event["msg"] == "repository.refresh.completed" {
			refreshes = append(refreshes, event)
		}
	}
	if len(refreshes) != 1 {
		t.Fatalf("refreshes = %#v", refreshes)
	}
	refresh := refreshes[0]
	seenOperation, seenDiagnostic := false, false
	for _, event := range events {
		switch event["msg"] {
		case "github.request.completed":
			seenOperation = true
		case "skipping non-chart GitHub release asset":
			seenDiagnostic = true
		default:
			continue
		}
		if event["trace_id"] != refresh["trace_id"] || event["refresh_id"] != refresh["refresh_id"] {
			t.Fatalf("correlation = %#v", event)
		}
	}
	if !seenOperation || !seenDiagnostic || refresh["trace_id"] != events[0]["trace_id"] {
		t.Fatalf("events = %#v", events)
	}
}

func TestAssembledDownloadCorrelatesGitHubOperation(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	transport := githubTransportFunc(func(r *http.Request) (*http.Response, error) {
		body := "chart bytes"
		if strings.HasSuffix(r.URL.Path, "/releases") {
			body = `[{"assets":[{"id":7,"name":"demo-1.0.0.tgz"}]}]`
		}
		return githubResponse(http.StatusOK, body, make(http.Header)), nil
	})
	runtime := app.New(releaseConfig(60), nil, logger, githubclient.NewClient(&http.Client{Transport: transport}, logger), fixedClock, context.Background())
	request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	output.Reset()
	response := request(runtime.Handler, http.MethodGet, "http://example.test/charts/releases/7/demo-1.0.0.tgz")
	events := decodeEvents(t, output.Bytes())
	if response.Code != http.StatusOK || response.Body.String() != "chart bytes" || len(events) != 3 {
		t.Fatalf("response = %d %q, events = %#v", response.Code, response.Body.String(), events)
	}
	trace := events[0]["trace_id"]
	if trace == nil || trace == "" {
		t.Fatalf("missing trace: %#v", events)
	}
	for i, name := range []string{"http.request", "github.request.completed", "http.response"} {
		if events[i]["msg"] != name || events[i]["trace_id"] != trace {
			t.Fatalf("events = %#v", events)
		}
	}
	if events[1]["source"] != "releases" {
		t.Fatalf("operation = %#v", events[1])
	}
}

func TestAssembledTokenRedactionPreservesPublicErrors(t *testing.T) {
	for _, discoveryFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "download", true: "discovery"}[discoveryFails], func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			cfg := releaseConfig(60)
			cfg.Sources[0].GitHubToken = "private-token"
			transport := githubTransportFunc(func(request *http.Request) (*http.Response, error) {
				if !discoveryFails && strings.HasSuffix(request.URL.Path, "/releases") {
					return githubResponse(200, `[{"assets":[{"id":7,"name":"demo-1.0.0.tgz"}]}]`, make(http.Header)), nil
				}
				return nil, errors.New("transport rejected private-token")
			})
			runtime := app.New(cfg, nil, logger, githubclient.NewClient(&http.Client{Transport: transport}, logger), fixedClock, context.Background())
			request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
			target := "http://example.test/charts/releases/7/demo-1.0.0.tgz"
			wantStatus := http.StatusBadGateway
			if discoveryFails {
				target = "http://example.test/status"
				wantStatus = http.StatusOK
			}
			response := request(runtime.Handler, http.MethodGet, target)
			if response.Code != wantStatus || !strings.Contains(response.Body.String(), "transport rejected private-token") {
				t.Fatalf("public response changed: %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(output.String(), "private-token") || !strings.Contains(output.String(), "token-redacted") {
				t.Fatalf("unsafe diagnostic: %s", output.String())
			}
		})
	}
}
