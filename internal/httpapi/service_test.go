package httpapi_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"helm-github-releases-proxy/internal/config"
	githubclient "helm-github-releases-proxy/internal/github"
	"helm-github-releases-proxy/internal/startup"
)

type fakeGitHubBackend struct {
	releases    []githubclient.Release
	asset       io.ReadCloser
	branch      string
	releasesErr error
	branchErr   error
	listCalls   int
}

func (f *fakeGitHubBackend) ListReleases(context.Context, string, string, string) ([]githubclient.Release, error) {
	f.listCalls++
	if f.releasesErr != nil {
		return nil, f.releasesErr
	}
	return f.releases, nil
}

func (f *fakeGitHubBackend) DownloadAsset(context.Context, string, string, int64, string) (io.ReadCloser, error) {
	return f.asset, nil
}

func (f *fakeGitHubBackend) FetchBranchFile(_ context.Context, _, _, _, path, _ string) (io.ReadCloser, error) {
	if f.branchErr != nil {
		return nil, f.branchErr
	}
	if path == "index.yaml" {
		return io.NopCloser(strings.NewReader(f.branch)), nil
	}
	return io.NopCloser(strings.NewReader("branch chart")), nil
}

func TestHTTPServiceStatusReportsRepositoriesCacheAndSanitizedFailures(t *testing.T) {
	localPath := filepath.Join(t.TempDir(), "private-charts")
	fake := &fakeGitHubBackend{releasesErr: errors.New("upstream failed at /container/secrets/token")}
	service := startup.New(config.Config{Repositories: []config.Repository{
		{Name: "github", Type: config.GitHubReleasesType, Owner: "acme", Repo: "charts"},
		{Name: "local", Type: config.LocalDirectoryType, Path: localPath},
	}}, nil, slog.Default(), fake, time.Now, context.Background())

	response := httptest.NewRecorder()
	service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, response.Code, http.StatusOK)

	statusResponse := httptest.NewRecorder()
	service.Handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/status", nil))
	assertStatusCode(t, statusResponse.Code, http.StatusOK)
	var got struct {
		Status       string `json:"status"`
		CachePresent bool   `json:"cache_present"`
		Repositories []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Owner  string `json:"owner"`
			Repo   string `json:"repo"`
			Path   string `json:"path"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "partial" || !got.CachePresent || len(got.Repositories) != 2 {
		t.Fatalf("status = %#v", got)
	}
	if got.Repositories[0].Owner != "acme" || got.Repositories[0].Repo != "charts" || got.Repositories[0].Status != "failed" || strings.Contains(got.Repositories[0].Error, "/container") {
		t.Fatalf("github status = %#v", got.Repositories[0])
	}
	if got.Repositories[1].Path != "private-charts" || strings.Contains(got.Repositories[1].Path, string(filepath.Separator)) {
		t.Fatalf("local path was not redacted: %#v", got.Repositories[1])
	}
}

func TestHTTPServiceDiscardsPartiallyParsedChartReleaserFailures(t *testing.T) {
	serviceBackend := &fakeGitHubBackend{branch: `apiVersion: v1
entries:
  good:
    - name: good
      version: 1.0.0
      urls: [good-1.0.0.tgz]
  bad:
    - name: bad
      urls: []
`}
	service := startup.New(config.Config{Repositories: []config.Repository{{
		Name: "pages", Type: config.ChartReleaserType, Owner: "acme", Repo: "charts", Branch: "gh-pages",
	}}}, nil, slog.Default(), serviceBackend, time.Now, context.Background())
	response := httptest.NewRecorder()
	service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, response.Code, http.StatusOK)
	if strings.Contains(response.Body.String(), "good:") || strings.Contains(response.Body.String(), "bad:") {
		t.Fatalf("failed backend contributed entries: %q", response.Body.String())
	}
}

func (f *fakeGitHubBackend) DownloadRelease(context.Context, string, string, string, string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("release chart")), nil
}

func fakeBackendForMode(mode string) *fakeGitHubBackend {
	if mode == "chart-releaser" {
		return &fakeGitHubBackend{branch: `apiVersion: v1
entries:
  remote:
    - name: remote
      version: 1.0.0
      urls: [remote-1.0.0.tgz]
`}
	}
	return &fakeGitHubBackend{releases: []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: "remote-1.0.0.tgz"}}}}}
}

func failBackendForMode(backend *fakeGitHubBackend, mode string) {
	if mode == "chart-releaser" {
		backend.branchErr = errors.New("GitHub unavailable")
		return
	}
	backend.releasesErr = errors.New("GitHub unavailable")
}

func TestHTTPServiceIndexesAndStreamsChartReleaserPackages(t *testing.T) {
	fake := &fakeGitHubBackend{branch: `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 1.2.3
      description: kept
      appVersion: "4.5"
      maintainers:
        - name: Maintainer
      unknown: discarded
      urls:
        - https://github.com/acme/charts/releases/download/v1.2.3/demo-1.2.3.tgz
        - packages/demo-1.2.3.tgz
`}
	service := startup.New(config.Config{Repositories: []config.Repository{{
		Name: "pages", Type: config.ChartReleaserType, Owner: "acme", Repo: "charts", Branch: "gh-pages",
	}}}, nil, slog.Default(), fake, time.Now, context.Background())
	index := httptest.NewRecorder()
	service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, index.Code, http.StatusOK)
	body := index.Body.String()
	if !strings.Contains(body, "description: kept") || !strings.Contains(body, "appVersion: \"4.5\"") || strings.Contains(body, "unknown:") {
		t.Fatalf("index = %q", body)
	}
	if !strings.Contains(body, "charts/pages/v1.2.3/demo-1.2.3.tgz") || !strings.Contains(body, "charts/pages/package-in-branch/packages/demo-1.2.3.tgz") {
		t.Fatalf("rewritten URLs missing: %q", body)
	}
	response := httptest.NewRecorder()
	service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/charts/pages/package-in-branch/packages/demo-1.2.3.tgz", nil))
	assertStatusCode(t, response.Code, http.StatusOK)
	if response.Body.String() != "branch chart" || response.Header().Get("Content-Disposition") != `attachment; filename="demo-1.2.3.tgz"` {
		t.Fatalf("response = (%d, %q, %#v)", response.Code, response.Body.String(), response.Header())
	}
}

func TestHTTPServiceShell(t *testing.T) {
	serviceClock := func() time.Time { return time.Date(2026, 8, 30, 12, 34, 56, 0, time.FixedZone("UTC", 0)) }
	service := startup.New(config.Config{Port: 8080}, nil, slog.Default(), githubclient.NewClient(nil), serviceClock, context.Background())
	server := httptest.NewServer(service.Handler)
	t.Cleanup(server.Close)

	t.Run("health", func(t *testing.T) {
		response, err := server.Client().Get(server.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		assertStatus(t, response, http.StatusOK)
		assertJSON(t, response, map[string]string{"status": "ok"})
	})

	t.Run("ready", func(t *testing.T) {
		response, err := server.Client().Get(server.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		assertStatus(t, response, http.StatusOK)
		assertJSON(t, response, map[string]string{"status": "ok"})
	})

	t.Run("empty index", func(t *testing.T) {
		response, err := server.Client().Get(server.URL + "/index.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		assertStatus(t, response, http.StatusOK)
		if got := response.Header.Get("Content-Type"); got != "application/x-yaml" {
			t.Fatalf("content type = %q", got)
		}
		if got := readBody(t, response); got != "apiVersion: v1\ngenerated: \"2026-08-30T12:34:56Z\"\nentries: {}\n" {
			t.Fatalf("body = %q", got)
		}
	})
}

func TestHTTPServiceReadinessReflectsConfigurationValidity(t *testing.T) {
	service := startup.New(config.Config{}, errors.New("invalid configuration"), slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	service.Handler.ServeHTTP(response, request)

	assertStatusCode(t, response.Code, http.StatusServiceUnavailable)
	if got := response.Body.String(); got != `{"status":"not_ready"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestHTTPServiceServesLocalChartsAndIndexesThem(t *testing.T) {
	directory := t.TempDir()
	filename := "demo-1.2.3.tgz"
	path := filepath.Join(directory, filename)
	writeTestChart(t, path, "demo", "1.2.3", "chart bytes")
	if err := os.Chtimes(path, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC), time.Time{}); err != nil {
		t.Fatal(err)
	}
	service := startup.New(config.Config{Repositories: []config.Repository{{Name: "local", Type: config.LocalDirectoryType, Path: directory}}}, nil, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	handler := service.Handler

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, index.Code, http.StatusOK)
	if !strings.Contains(index.Body.String(), "demo-1.2.3.tgz") || !strings.Contains(index.Body.String(), "digest:") {
		t.Fatalf("index = %q", index.Body.String())
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/charts/local/"+filename, nil))
	assertStatusCode(t, response.Code, http.StatusOK)
	if response.Header().Get("Content-Type") != "application/gzip" || response.Header().Get("Content-Disposition") != `attachment; filename="demo-1.2.3.tgz"` {
		t.Fatalf("headers = %#v", response.Header())
	}
	if len(response.Body.Bytes()) == 0 || response.Body.Bytes()[0] != 0x1f || response.Body.Bytes()[1] != 0x8b {
		t.Fatalf("body is not a gzip archive")
	}
}

func TestHTTPServiceIndexesAndStreamsGitHubReleaseAssets(t *testing.T) {
	created := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := &fakeGitHubBackend{
		releases: []githubclient.Release{{PublishedAt: &created, Assets: []githubclient.Asset{
			{ID: 42, Name: "demo-v1.2.3.tgz", Digest: "sha256:abc", APIURL: "https://api.github.test/assets/42"},
			{ID: 43, Name: "demo-1.2.tgz"},
			{ID: 44, Name: "checksums.txt"},
		}}},
		asset: io.NopCloser(strings.NewReader("github chart")),
	}
	service := startup.New(config.Config{Repositories: []config.Repository{{
		Name: "upstream", Type: config.GitHubReleasesType, Owner: "acme", Repo: "charts",
	}}}, nil, slog.Default(), fake, time.Now, context.Background())

	index := httptest.NewRecorder()
	service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, index.Code, http.StatusOK)
	body := index.Body.String()
	if !strings.Contains(body, "charts/upstream/42/demo-v1.2.3.tgz") || !strings.Contains(body, "version: 1.2.3") || !strings.Contains(body, "digest: abc") {
		t.Fatalf("index = %q", body)
	}
	if strings.Contains(body, "type: library") || strings.Contains(body, "demo-1.2.tgz") || strings.Contains(body, "checksums.txt") {
		t.Fatalf("index contains invalid or invented metadata = %q", body)
	}

	response := httptest.NewRecorder()
	service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/charts/upstream/42/demo-v1.2.3.tgz", nil))
	assertStatusCode(t, response.Code, http.StatusOK)
	if response.Body.String() != "github chart" || response.Header().Get("Content-Type") != "application/gzip" || response.Header().Get("Content-Disposition") != `attachment; filename="demo-v1.2.3.tgz"` {
		t.Fatalf("response = (%d, %q, %#v)", response.Code, response.Body.String(), response.Header())
	}
}

func TestHTTPServiceAggregatesLocalRepositoriesDeterministically(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeTestChart(t, filepath.Join(first, "zeta-1.0.0.tgz"), "zeta", "1.0.0", "first")
	writeTestChart(t, filepath.Join(first, "demo-1.0.0.tgz"), "demo", "1.0.0", "first-demo")
	writeTestChart(t, filepath.Join(first, "demo-2.0.0-beta.1.tgz"), "demo", "2.0.0-beta.1", "prerelease")
	writeTestChart(t, filepath.Join(second, "demo-1.0.0.tgz"), "demo", "1.0.0", "second-demo")
	writeTestChart(t, filepath.Join(second, "alpha-1.0.0.tgz"), "alpha", "1.0.0", "alpha")

	serviceClock := func() time.Time { return time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC) }
	service := startup.New(config.Config{Repositories: []config.Repository{
		{Name: "first", Type: config.LocalDirectoryType, Path: first},
		{Name: "second", Type: config.LocalDirectoryType, Path: second},
	}}, nil, slog.Default(), githubclient.NewClient(nil), serviceClock, context.Background())
	response := httptest.NewRecorder()
	service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, response.Code, http.StatusOK)

	body := response.Body.String()
	if !strings.Contains(body, "alpha:") || !strings.Contains(body, "demo-2.0.0-beta.1.tgz") || !strings.Contains(body, "- charts/first/demo-1.0.0.tgz\n            - charts/second/demo-1.0.0.tgz") {
		t.Fatalf("index = %q", body)
	}
	if strings.Index(body, "alpha:") > strings.Index(body, "demo:") || strings.Index(body, "demo:") > strings.Index(body, "zeta:") {
		t.Fatalf("chart names are not sorted: %q", body)
	}
	demo := body[strings.Index(body, "    demo:"):strings.Index(body, "    zeta:")]
	if strings.Index(demo, "version: 2.0.0-beta.1") > strings.Index(demo, "version: 1.0.0") {
		t.Fatalf("versions are not sorted: %q", body)
	}
	if strings.Count(demo, "name: demo") != 2 || strings.Count(demo, "charts/first/demo-1.0.0.tgz") != 1 || strings.Count(demo, "charts/second/demo-1.0.0.tgz") != 1 {
		t.Fatalf("duplicate versions were not merged: %q", demo)
	}
	if strings.Contains(body, "appVersion:") || strings.Contains(body, "description:") || strings.Contains(body, "home:") {
		t.Fatalf("empty metadata was emitted: %q", body)
	}
}

func TestHTTPServiceServesLastSuccessfulIndexWhenAllLocalRepositoriesFail(t *testing.T) {
	directory := t.TempDir()
	writeTestChart(t, filepath.Join(directory, "demo-1.0.0.tgz"), "demo", "1.0.0", "chart")
	service := startup.New(config.Config{Repositories: []config.Repository{{Name: "local", Type: config.LocalDirectoryType, Path: directory}}}, nil, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	first := httptest.NewRecorder()
	service.Handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	service.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if second.Code != http.StatusOK || second.Body.String() != first.Body.String() {
		t.Fatalf("stale response = (%d, %q), want (%d, %q)", second.Code, second.Body.String(), first.Code, first.Body.String())
	}
}

func TestHTTPServiceServesLastSuccessfulIndexWhenOneLocalRepositoryFails(t *testing.T) {
	working := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing")
	writeTestChart(t, filepath.Join(working, "demo-1.0.0.tgz"), "demo", "1.0.0", "chart")
	service := startup.New(config.Config{Repositories: []config.Repository{
		{Name: "working", Type: config.LocalDirectoryType, Path: working},
		{Name: "missing", Type: config.LocalDirectoryType, Path: missing},
	}}, nil, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	first := httptest.NewRecorder()
	service.Handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "charts/working/demo-1.0.0.tgz") {
		t.Fatalf("first response = (%d, %q)", first.Code, first.Body.String())
	}
	if err := os.RemoveAll(working); err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	service.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if second.Code != http.StatusOK || second.Body.String() != first.Body.String() {
		t.Fatalf("stale response = (%d, %q), want (%d, %q)", second.Code, second.Body.String(), first.Code, first.Body.String())
	}
}

func TestHTTPServiceCachesIndexUntilTTLExpires(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "demo-1.0.0.tgz")
	writeTestChart(t, path, "demo", "1.0.0", "first")
	current := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	serviceClock := func() time.Time { return current }
	service := startup.New(config.Config{
		CacheTTLSeconds: 60,
		Repositories:    []config.Repository{{Name: "local", Type: config.LocalDirectoryType, Path: directory}},
	}, nil, slog.Default(), githubclient.NewClient(nil), serviceClock, context.Background())
	handler := service.Handler

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "demo-1.0.0.tgz") {
		t.Fatalf("first response = (%d, %q)", first.Code, first.Body.String())
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}

	current = current.Add(30 * time.Second)
	withinTTL := httptest.NewRecorder()
	handler.ServeHTTP(withinTTL, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if withinTTL.Code != http.StatusOK || withinTTL.Body.String() != first.Body.String() {
		t.Fatalf("cached response = (%d, %q), want %q", withinTTL.Code, withinTTL.Body.String(), first.Body.String())
	}

	current = current.Add(31 * time.Second)
	afterTTL := httptest.NewRecorder()
	handler.ServeHTTP(afterTTL, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	if afterTTL.Code != http.StatusOK || afterTTL.Body.String() != first.Body.String() {
		t.Fatalf("stale response = (%d, %q), want (%d, %q)", afterTTL.Code, afterTTL.Body.String(), first.Code, first.Body.String())
	}
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(status.Body.String(), `"stale":true`) || !strings.Contains(status.Body.String(), `"last_error":"repositories failed: local"`) {
		t.Fatalf("status = %q", status.Body.String())
	}
}

func TestHTTPServiceStartupWarmIsAsynchronousAndDoesNotAffectReadiness(t *testing.T) {
	directory := t.TempDir()
	writeTestChart(t, filepath.Join(directory, "demo-1.0.0.tgz"), "demo", "1.0.0", "chart")
	service := startup.New(config.Config{Repositories: []config.Repository{{Name: "local", Type: config.LocalDirectoryType, Path: directory}}}, nil, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	service.Start()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := httptest.NewRecorder()
		service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
		if strings.Contains(status.Body.String(), `"last_success_at":"`) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("startup warm did not complete")
}

func writeTestChart(t *testing.T, path, name, version, content string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	metadata := "name: " + name + "\nversion: " + version + "\n"
	if err := tarWriter.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(metadata))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(metadata)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: name + "/content.txt", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPServiceLocalDownloadsRejectUnindexedAndTraversalPaths(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "demo-1.2.3.tgz"), []byte("chart"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := startup.New(config.Config{Repositories: []config.Repository{{Name: "local", Type: config.LocalDirectoryType, Path: directory}}}, nil, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	for _, requestPath := range []string{"/charts/local/demo-1.2.3.tgz", "/charts/local/../demo-1.2.3.tgz", "/charts/local/%2e%2e/demo-1.2.3.tgz"} {
		response := httptest.NewRecorder()
		service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		assertStatusCode(t, response.Code, http.StatusNotFound)
		if response.Header().Get("Content-Type") == "application/gzip" {
			t.Fatalf("unexpected chart content type for %q", requestPath)
		}
	}
}

func assertStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("status = %d, want %d", response.StatusCode, want)
	}
}

func assertStatusCode(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func assertJSON(t *testing.T, response *http.Response, want map[string]string) {
	t.Helper()
	var got map[string]string
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got["status"] != want["status"] {
		t.Fatalf("json = %#v, want %#v", got, want)
	}
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFlatConfigurationAggregatesLocalChartsWithGitHubModes(t *testing.T) {
	created := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		mode, githubURL, githubBody, description, inactiveRoute string
		backend                                                 startup.GitHubBackend
	}{
		{
			mode: "github-releases", githubURL: "charts/github-releases/7/demo-1.2.3.tgz",
			githubBody: "GitHub release chart", inactiveRoute: "/charts/chart-releaser/v1.2.3/demo-1.2.3.tgz",
			backend: &fakeGitHubBackend{
				releases: []githubclient.Release{{PublishedAt: &created, Assets: []githubclient.Asset{
					{ID: 8, Name: "demo-2.0.0.tgz"},
					{ID: 7, Name: "demo-1.2.3.tgz", Digest: "sha256:github-digest"},
				}}},
				asset: io.NopCloser(strings.NewReader("GitHub release chart")),
			},
		},
		{
			mode: "chart-releaser", githubURL: "charts/chart-releaser/v1.2.3/demo-1.2.3.tgz",
			githubBody: "release chart", description: "GitHub duplicate metadata",
			inactiveRoute: "/charts/github-releases/7/demo-1.2.3.tgz",
			backend: &fakeGitHubBackend{branch: `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 2.0.0
      description: GitHub distinct metadata
      urls: [https://github.com/acme/charts/releases/download/v2.0.0/demo-2.0.0.tgz]
    - apiVersion: v2
      name: demo
      version: 1.2.3
      description: GitHub duplicate metadata
      digest: github-digest
      urls: [https://github.com/acme/charts/releases/download/v1.2.3/demo-1.2.3.tgz]
`},
		},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			directory := t.TempDir()
			writeTestChart(t, filepath.Join(directory, "demo-1.2.3.tgz"), "demo", "1.2.3", "local duplicate")
			writeTestChart(t, filepath.Join(directory, "demo-1.0.0.tgz"), "demo", "1.0.0", "local distinct")
			cfg, err := config.Load(func(name string) string {
				return map[string]string{
					"MODE": tc.mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "LOCAL_PATH": directory,
				}[name]
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Repositories) != 2 || cfg.Repositories[1].Name != "local" {
				t.Fatalf("sources = %#v", cfg.Repositories)
			}

			service := startup.New(cfg, err, slog.Default(), tc.backend, time.Now, context.Background())

			handler := service.Handler
			index := httptest.NewRecorder()
			handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
			assertStatusCode(t, index.Code, http.StatusOK)
			var catalog struct {
				Entries map[string][]struct {
					Version     string   `yaml:"version"`
					Description string   `yaml:"description"`
					Digest      string   `yaml:"digest"`
					URLs        []string `yaml:"urls"`
				} `yaml:"entries"`
			}
			if err := yaml.Unmarshal(index.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			entries := catalog.Entries["demo"]
			if len(entries) != 3 || entries[0].Version != "2.0.0" || entries[1].Version != "1.2.3" || entries[2].Version != "1.0.0" {
				t.Fatalf("version order = %#v", entries)
			}
			if got := entries[1].URLs; len(got) != 2 || got[0] != tc.githubURL || got[1] != "charts/local/demo-1.2.3.tgz" {
				t.Fatalf("duplicate URLs = %#v", got)
			}
			if entries[1].Digest != "github-digest" {
				t.Fatalf("duplicate metadata = %#v", entries[1])
			}
			if entries[1].Description != tc.description {
				t.Fatalf("duplicate metadata = %#v", entries[1])
			}

			for path, wantBody := range map[string]string{
				"/" + tc.githubURL:             tc.githubBody,
				"/charts/local/demo-1.2.3.tgz": "",
			} {
				download := httptest.NewRecorder()
				handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, path, nil))
				assertStatusCode(t, download.Code, http.StatusOK)
				if wantBody != "" && download.Body.String() != wantBody {
					t.Fatalf("GET %s body = %q, want %q", path, download.Body.String(), wantBody)
				}
				if download.Header().Get("Content-Type") != "application/gzip" {
					t.Fatalf("GET %s content type = %q", path, download.Header().Get("Content-Type"))
				}
			}

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.inactiveRoute, nil))
			assertStatusCode(t, response.Code, http.StatusNotFound)
		})
	}
}

func TestFlatConfigurationGitHubModesKeepResultsWhenLocalSourceIsEmptyOrUnavailable(t *testing.T) {
	for _, mode := range []string{"github-releases", "chart-releaser"} {
		for _, localState := range []string{"empty", "unavailable"} {
			t.Run(mode+"/"+localState, func(t *testing.T) {
				directory := t.TempDir()
				if localState == "unavailable" {
					directory = filepath.Join(directory, "missing")
				}
				cfg, err := config.Load(func(name string) string {
					return map[string]string{
						"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "LOCAL_PATH": directory,
					}[name]
				})
				if err != nil {
					t.Fatal(err)
				}
				service := startup.New(cfg, err, slog.Default(), fakeBackendForMode(mode), time.Now, context.Background())
				index := httptest.NewRecorder()
				service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
				assertStatusCode(t, index.Code, http.StatusOK)
				if !strings.Contains(index.Body.String(), "charts/"+mode+"/") {
					t.Fatalf("GitHub entry missing from index: %s", index.Body.String())
				}
				status := httptest.NewRecorder()
				service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
				want := `"status":"ok"`
				if localState == "unavailable" {
					want = `"status":"partial"`
				}
				if !strings.Contains(status.Body.String(), want) || !strings.Contains(status.Body.String(), `"name":"local"`) {
					t.Fatalf("status = %s, want %s and local source", status.Body.String(), want)
				}
			})
		}
	}
}

func TestFlatConfigurationGitHubModesPreserveCombinedSourceFailureBehavior(t *testing.T) {
	for _, mode := range []string{"github-releases", "chart-releaser"} {
		t.Run(mode+" partial local result", func(t *testing.T) {
			directory := t.TempDir()
			writeTestChart(t, filepath.Join(directory, "local-1.0.0.tgz"), "local", "1.0.0", "local chart")
			cfg, err := config.Load(func(name string) string {
				return map[string]string{
					"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "LOCAL_PATH": directory,
				}[name]
			})
			if err != nil {
				t.Fatal(err)
			}
			backend := fakeBackendForMode(mode)
			failBackendForMode(backend, mode)
			service := startup.New(cfg, err, slog.Default(), backend, time.Now, context.Background())

			index := httptest.NewRecorder()
			service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
			assertStatusCode(t, index.Code, http.StatusOK)
			if !strings.Contains(index.Body.String(), "charts/local/local-1.0.0.tgz") || strings.Contains(index.Body.String(), "charts/"+mode+"/") {
				t.Fatalf("partial index = %s", index.Body.String())
			}
			status := httptest.NewRecorder()
			service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
			if !strings.Contains(status.Body.String(), `"status":"partial"`) || !strings.Contains(status.Body.String(), `"name":"`+mode+`"`) || !strings.Contains(status.Body.String(), `"name":"local"`) {
				t.Fatalf("partial status = %s", status.Body.String())
			}
		})

		t.Run(mode+" stale fallback when both sources fail", func(t *testing.T) {
			directory := t.TempDir()
			writeTestChart(t, filepath.Join(directory, "local-1.0.0.tgz"), "local", "1.0.0", "local chart")
			cfg, err := config.Load(func(name string) string {
				return map[string]string{
					"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "LOCAL_PATH": directory,
					"CACHE_TTL_SECONDS": "0",
				}[name]
			})
			if err != nil {
				t.Fatal(err)
			}
			backend := fakeBackendForMode(mode)
			service := startup.New(cfg, err, slog.Default(), backend, time.Now, context.Background())
			first := httptest.NewRecorder()
			service.Handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
			assertStatusCode(t, first.Code, http.StatusOK)
			if !strings.Contains(first.Body.String(), "charts/"+mode+"/") || !strings.Contains(first.Body.String(), "charts/local/") {
				t.Fatalf("successful aggregate = %s", first.Body.String())
			}

			failBackendForMode(backend, mode)
			if err := os.RemoveAll(directory); err != nil {
				t.Fatal(err)
			}
			second := httptest.NewRecorder()
			service.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
			assertStatusCode(t, second.Code, http.StatusOK)
			if second.Body.String() != first.Body.String() {
				t.Fatalf("fallback index = %s, want %s", second.Body.String(), first.Body.String())
			}
			status := httptest.NewRecorder()
			service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
			if !strings.Contains(status.Body.String(), `"status":"stale"`) || !strings.Contains(status.Body.String(), `"last_error":"repositories failed: `+mode+`, local"`) {
				t.Fatalf("stale status = %s", status.Body.String())
			}
		})
	}
}

func TestFlatConfigurationServesGitHubReleases(t *testing.T) {
	for _, mode := range []string{"", "github-releases"} {
		for _, token := range []string{"unset", "", "secret"} {
			t.Run("mode="+mode+"/token="+token, func(t *testing.T) {
				values := map[string]string{"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "CHART_RELEASER_PAGES_BRANCH": "ignored"}
				wantAuth := ""
				if token != "unset" {
					values["GITHUB_TOKEN"] = token
				}
				if token == "secret" {
					wantAuth = "Bearer secret"
				}
				cfg, err := config.Load(func(name string) string { return values[name] })
				if err != nil {
					t.Fatal(err)
				}
				requests := 0
				serviceBackend := githubclient.NewClient(&http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
					requests++
					if got := r.Header.Get("Authorization"); got != wantAuth {
						t.Errorf("Authorization = %q, want %q", got, wantAuth)
					}
					body := ""
					switch r.URL.Path {
					case "/repos/acme/charts/releases":
						body = `[{"assets":[{"id":7,"name":"demo-1.0.0.tgz"},{"id":8,"name":"notes.txt"}]}]`
					case "/repos/acme/charts/releases/assets/7":
						if r.Header.Get("Accept") != "application/octet-stream" {
							t.Error("asset request must use octet-stream")
						}
						body = "chart bytes"
					default:
						t.Errorf("unexpected upstream request: %s", r.URL)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})})
				service := startup.New(cfg, err, slog.Default(), serviceBackend, time.Now, context.Background())
				handler := service.Handler
				index := httptest.NewRecorder()
				handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
				assertStatusCode(t, index.Code, http.StatusOK)
				var catalog struct {
					Entries map[string][]struct {
						Name    string   `yaml:"name"`
						Version string   `yaml:"version"`
						URLs    []string `yaml:"urls"`
					} `yaml:"entries"`
				}
				if err := yaml.Unmarshal(index.Body.Bytes(), &catalog); err != nil {
					t.Fatal(err)
				}
				if len(catalog.Entries) != 1 || len(catalog.Entries["demo"]) != 1 {
					t.Fatalf("index = %s", index.Body.String())
				}
				chart := catalog.Entries["demo"][0]
				if chart.Name != "demo" || chart.Version != "1.0.0" || len(chart.URLs) != 1 || chart.URLs[0] != "charts/github-releases/7/demo-1.0.0.tgz" {
					t.Fatalf("entry = %#v", chart)
				}
				download := httptest.NewRecorder()
				handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/"+chart.URLs[0], nil))
				if download.Code != http.StatusOK || download.Body.String() != "chart bytes" || download.Header().Get("Content-Type") != "application/gzip" || download.Header().Get("Content-Disposition") != `attachment; filename="demo-1.0.0.tgz"` {
					t.Fatalf("download = %#v", download)
				}
				for _, path := range []string{"/charts/chart-releaser/v1/demo-1.0.0.tgz", "/charts/chart-releaser/package-in-branch/demo-1.0.0.tgz", "/charts/local/demo-1.0.0.tgz", "/charts/github/7/demo-1.0.0.tgz", "/charts/github-releases/8/notes.txt", "/charts/github-releases/7/wrong-1.0.0.tgz"} {
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
					assertStatusCode(t, response.Code, http.StatusNotFound)
				}
				if requests != 2 {
					t.Fatalf("upstream requests = %d, want 2", requests)
				}
			})
		}
	}
}

func TestFlatConfigurationServesLocalOnlyChartsWithoutGitHub(t *testing.T) {
	directory := t.TempDir()
	filename := "demo-1.2.3.tgz"
	writeTestChart(t, filepath.Join(directory, filename), "demo", "1.2.3", "local chart")
	values := map[string]string{
		"MODE":                        "local-only",
		"LOCAL_PATH":                  directory,
		"GITHUB_OWNER":                "ignored-owner",
		"GITHUB_REPO":                 "ignored-repository",
		"GITHUB_TOKEN":                "ignored-token",
		"CHART_RELEASER_PAGES_BRANCH": "ignored-branch",
		"CACHE_TTL_SECONDS":           "0",
	}
	cfg, err := config.Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeGitHubBackend{releasesErr: errors.New("GitHub must not be contacted")}
	service := startup.New(cfg, err, slog.Default(), fake, time.Now, context.Background())
	service.Start()

	deadline := time.Now().Add(time.Second)
	warmed := false
	for time.Now().Before(deadline) {
		status := httptest.NewRecorder()
		service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
		if strings.Contains(status.Body.String(), `"last_success_at":"`) {
			warmed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !warmed {
		t.Fatal("local-only startup warm did not complete")
	}
	if fake.listCalls != 0 {
		t.Fatalf("GitHub calls during startup warm = %d", fake.listCalls)
	}

	index := httptest.NewRecorder()
	service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, index.Code, http.StatusOK)
	var catalog struct {
		Entries map[string][]struct {
			URLs []string `yaml:"urls"`
		} `yaml:"entries"`
	}
	if err := yaml.Unmarshal(index.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Entries["demo"]) != 1 || len(catalog.Entries["demo"][0].URLs) != 1 || catalog.Entries["demo"][0].URLs[0] != "charts/local/"+filename {
		t.Fatalf("index = %s", index.Body.String())
	}

	download := httptest.NewRecorder()
	service.Handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/charts/local/"+filename, nil))
	assertStatusCode(t, download.Code, http.StatusOK)
	if download.Header().Get("Content-Type") != "application/gzip" || len(download.Body.Bytes()) < 2 || download.Body.Bytes()[0] != 0x1f || download.Body.Bytes()[1] != 0x8b {
		t.Fatalf("download = (%q, %q)", download.Header().Get("Content-Type"), download.Body.String())
	}
	for _, path := range []string{
		"/charts/github-releases/7/demo-1.2.3.tgz",
		"/charts/chart-releaser/v1.2.3/demo-1.2.3.tgz",
		"/charts/chart-releaser/package-in-branch/demo-1.2.3.tgz",
	} {
		response := httptest.NewRecorder()
		service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assertStatusCode(t, response.Code, http.StatusNotFound)
	}
	if fake.listCalls != 0 {
		t.Fatalf("GitHub calls = %d", fake.listCalls)
	}
}

func TestFlatConfigurationServesEmptyLocalOnlyDirectory(t *testing.T) {
	directory := t.TempDir()
	cfg, err := config.Load(func(name string) string {
		return map[string]string{"MODE": "local-only", "LOCAL_PATH": directory}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	service := startup.New(cfg, err, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	index := httptest.NewRecorder()
	service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, index.Code, http.StatusOK)
	if !strings.Contains(index.Body.String(), "entries: {}") {
		t.Fatalf("index = %q", index.Body.String())
	}
	status := httptest.NewRecorder()
	service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(status.Body.String(), `"name":"local"`) || !strings.Contains(status.Body.String(), `"indexed_count":0`) || !strings.Contains(status.Body.String(), `"status":"ok"`) {
		t.Fatalf("status = %s", status.Body.String())
	}
}

func TestFlatConfigurationReportsUnavailableLocalOnlyDirectoryAsSourceFailure(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	cfg, err := config.Load(func(name string) string {
		return map[string]string{"MODE": "local-only", "LOCAL_PATH": directory}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	service := startup.New(cfg, err, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	index := httptest.NewRecorder()
	service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, index.Code, http.StatusOK)
	status := httptest.NewRecorder()
	service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(status.Body.String(), `"status":"partial"`) || !strings.Contains(status.Body.String(), `"last_error":"repositories failed: local"`) || !strings.Contains(status.Body.String(), `"name":"local"`) || !strings.Contains(status.Body.String(), `"status":"failed"`) {
		t.Fatalf("status = %s", status.Body.String())
	}
}

func TestFlatConfigurationServesChartReleaser(t *testing.T) {
	for _, tc := range []struct {
		name, branch, token, wantAuth string
	}{
		{name: "default branch"},
		{name: "custom branch with token", branch: "release-pages", token: "secret", wantAuth: "Bearer secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{
				"MODE": "chart-releaser", "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts",
			}
			wantBranch := "gh-pages"
			if tc.branch != "" {
				values["CHART_RELEASER_PAGES_BRANCH"] = tc.branch
				wantBranch = tc.branch
			}
			if tc.token != "" {
				values["GITHUB_TOKEN"] = tc.token
			}
			cfg, err := config.Load(func(name string) string { return values[name] })
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			serviceBackend := githubclient.NewClient(&http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				body := ""
				switch {
				case r.URL.Host == "api.github.com" && r.URL.Path == "/repos/acme/charts/contents/index.yaml":
					if r.URL.Query().Get("ref") != wantBranch || r.Header.Get("Authorization") != tc.wantAuth {
						t.Errorf("index request = %s, Authorization = %q", r.URL, r.Header.Get("Authorization"))
					}
					body = `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 1.2.3
      appVersion: "4.5"
      description: preserved description
      home: https://example.test/demo
      icon: https://example.test/icon.svg
      deprecated: false
      keywords: [example]
      maintainers:
        - name: Maintainer
          email: maintainer@example.test
      sources: [https://example.test/source]
      annotations:
        example.test/channel: stable
      created: "2026-09-08T12:00:00Z"
      digest: sha256:abc123
      urls:
        - https://github.com/acme/charts/releases/download/v1.2.3/demo-1.2.3.tgz
        - demo-root-1.2.3.tgz
        - packages/nested/demo-1.2.3.tgz
`
				case r.URL.Host == "github.com" && r.URL.Path == "/acme/charts/releases/download/v1.2.3/demo-1.2.3.tgz":
					if r.Header.Get("Authorization") != tc.wantAuth {
						t.Errorf("release download Authorization = %q, want %q", r.Header.Get("Authorization"), tc.wantAuth)
					}
					body = "release chart"
				case r.URL.Host == "api.github.com" && r.URL.Path == "/repos/acme/charts/contents/demo-root-1.2.3.tgz":
					if r.URL.Query().Get("ref") != wantBranch || r.Header.Get("Authorization") != tc.wantAuth {
						t.Errorf("root branch package request = %s, Authorization = %q", r.URL, r.Header.Get("Authorization"))
					}
					body = "branch root chart"
				case r.URL.Host == "api.github.com" && r.URL.Path == "/repos/acme/charts/contents/packages/nested/demo-1.2.3.tgz":
					if r.URL.Query().Get("ref") != wantBranch || r.Header.Get("Authorization") != tc.wantAuth {
						t.Errorf("branch package request = %s, Authorization = %q", r.URL, r.Header.Get("Authorization"))
					}
					body = "branch chart"
				default:
					t.Errorf("unexpected upstream request: %s", r.URL)
					return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("not found"))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})})
			service := startup.New(cfg, err, slog.Default(), serviceBackend, time.Now, context.Background())
			handler := service.Handler

			index := httptest.NewRecorder()
			handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
			assertStatusCode(t, index.Code, http.StatusOK)
			body := index.Body.String()
			for _, expected := range []string{
				"appVersion: \"4.5\"", "description: preserved description", "home: https://example.test/demo",
				"icon: https://example.test/icon.svg", "deprecated: false", "keywords:", "maintainers:", "sources:",
				"example.test/channel: stable", "created: \"2026-09-08T12:00:00Z\"", "digest: sha256:abc123",
				"charts/chart-releaser/v1.2.3/demo-1.2.3.tgz",
				"charts/chart-releaser/package-in-branch/demo-root-1.2.3.tgz",
				"charts/chart-releaser/package-in-branch/packages/nested/demo-1.2.3.tgz",
			} {
				if !strings.Contains(body, expected) {
					t.Errorf("index missing %q:\n%s", expected, body)
				}
			}

			for path, wantBody := range map[string]string{
				"/charts/chart-releaser/v1.2.3/demo-1.2.3.tgz":                            "release chart",
				"/charts/chart-releaser/package-in-branch/demo-root-1.2.3.tgz":            "branch root chart",
				"/charts/chart-releaser/package-in-branch/packages/nested/demo-1.2.3.tgz": "branch chart",
			} {
				download := httptest.NewRecorder()
				handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, path, nil))
				if download.Code != http.StatusOK || download.Body.String() != wantBody || download.Header().Get("Content-Type") != "application/gzip" {
					t.Errorf("GET %s = (%d, %q, %#v)", path, download.Code, download.Body.String(), download.Header())
				}
			}

			for _, path := range []string{
				"/charts/github-releases/7/demo-1.2.3.tgz",
				"/charts/chart-releaser/package-in-branch/%2e%2e/demo-1.2.3.tgz",
				"/charts/chart-releaser/package-in-branch/packages/",
			} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				assertStatusCode(t, response.Code, http.StatusNotFound)
			}
			if requests != 4 {
				t.Fatalf("upstream requests = %d, want 4", requests)
			}
		})
	}
}

func TestFlatConfigurationErrorsKeepHealthAndReadiness(t *testing.T) {
	for _, values := range []map[string]string{
		{},
		{"GITHUB_OWNER": "acme"},
		{"GITHUB_REPO": "charts"},
		{"MODE": "chart-releaser", "GITHUB_OWNER": "acme"},
		{"MODE": "chart-releaser", "GITHUB_REPO": "charts"},
		{"MODE": "unknown", "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts"},
	} {
		cfg, err := config.Load(func(name string) string { return values[name] })
		if err == nil {
			t.Fatal("expected configuration error")
		}
		service := startup.New(cfg, err, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
		service.Start()
		for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusServiceUnavailable} {
			response := httptest.NewRecorder()
			service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			assertStatusCode(t, response.Code, want)
		}
	}
}

func TestFlatConfigurationGitHubCacheAndFailureFallback(t *testing.T) {
	for _, ttl := range []string{"0", "60"} {
		t.Run("ttl="+ttl, func(t *testing.T) {
			cfg, err := config.Load(func(name string) string {
				return map[string]string{"GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "CACHE_TTL_SECONDS": ttl}[name]
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
			serviceClock := func() time.Time { return now }
			backend := &fakeGitHubBackend{releases: []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: "demo-1.0.0.tgz"}}}}}
			service := startup.New(cfg, err, slog.Default(), backend, serviceClock, context.Background())
			get := func(path string) *httptest.ResponseRecorder {
				t.Helper()
				response := httptest.NewRecorder()
				service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				assertStatusCode(t, response.Code, http.StatusOK)
				return response
			}
			first := get("/index.yaml").Body.String()
			backend.releasesErr = errors.New("upstream unavailable")
			if got := get("/index.yaml").Body.String(); got != first {
				t.Fatalf("fallback index = %q, want %q", got, first)
			}
			wantState := `"status":"ok"`
			if ttl == "0" {
				wantState = `"status":"stale"`
			}
			if status := get("/status").Body.String(); !strings.Contains(status, wantState) {
				t.Fatalf("status = %s, want %s", status, wantState)
			}
			now = now.Add(61 * time.Second)
			if got := get("/index.yaml").Body.String(); got != first {
				t.Fatalf("expired fallback index = %q", got)
			}
			if status := get("/status").Body.String(); !strings.Contains(status, `"status":"stale"`) || !strings.Contains(status, `"last_error":"repositories failed: github-releases"`) {
				t.Fatalf("status = %s", status)
			}
		})
	}
}

func TestFlatConfigurationWarmsGitHubAsynchronously(t *testing.T) {
	cfg, err := config.Load(func(name string) string {
		return map[string]string{"GITHUB_OWNER": "acme", "GITHUB_REPO": "charts"}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	serviceBackend := githubclient.NewClient(&http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`[{"assets":[{"id":7,"name":"demo-1.0.0.tgz"}]}]`))}, nil
	})})
	service := startup.New(cfg, err, slog.Default(), serviceBackend, time.Now, context.Background())
	service.Start()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not query GitHub")
	}
	ready := httptest.NewRecorder()
	service.Handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assertStatusCode(t, ready.Code, http.StatusOK)
	unblock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := httptest.NewRecorder()
		service.Handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
		if strings.Contains(status.Body.String(), `"last_success_at":"`) {
			index := httptest.NewRecorder()
			service.Handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
			if !strings.Contains(index.Body.String(), "charts/github-releases/7/demo-1.0.0.tgz") {
				t.Fatalf("warmed index = %s", index.Body.String())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("startup warm did not complete")
}

func TestGitHubPackageMembershipFollowsAggregatePublication(t *testing.T) {
	directory := t.TempDir()
	backend := &fakeGitHubBackend{
		releases: []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: "demo-1.0.0.tgz"}}}},
		asset:    io.NopCloser(strings.NewReader("published package")),
	}
	service := startup.New(config.Config{Repositories: []config.Repository{
		{Name: "upstream", Type: config.GitHubReleasesType, Owner: "acme", Repo: "charts"},
		{Name: "local", Type: config.LocalDirectoryType, Path: directory},
	}}, nil, slog.Default(), backend, time.Now, context.Background())
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		service.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	first := get("/index.yaml")
	assertStatusCode(t, first.Code, http.StatusOK)
	backend.releases = []githubclient.Release{{Assets: []githubclient.Asset{
		{ID: 7, Name: "renamed-1.0.0.tgz"},
		{ID: 8, Name: "demo-2.0.0.tgz"},
	}}}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	fallback := get("/index.yaml")
	assertStatusCode(t, fallback.Code, http.StatusOK)
	if fallback.Body.String() != first.Body.String() {
		t.Fatalf("fallback replaced published index: %s", fallback.Body.String())
	}
	published := get("/charts/upstream/7/demo-1.0.0.tgz")
	assertStatusCode(t, published.Code, http.StatusOK)
	if published.Body.String() != "published package" {
		t.Fatalf("package = %q", published.Body.String())
	}
	for _, path := range []string{
		"/charts/upstream/7/renamed-1.0.0.tgz",
		"/charts/upstream/8/demo-2.0.0.tgz",
		"/charts/upstream/0/demo-1.0.0.tgz",
		"/charts/upstream/invalid/demo-1.0.0.tgz",
		"/charts/inactive/7/demo-1.0.0.tgz",
	} {
		t.Run(path, func(t *testing.T) { assertStatusCode(t, get(path).Code, http.StatusNotFound) })
	}
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	assertStatusCode(t, get("/index.yaml").Code, http.StatusOK)
	assertStatusCode(t, get("/charts/upstream/7/demo-1.0.0.tgz").Code, http.StatusNotFound)
	assertStatusCode(t, get("/charts/upstream/8/demo-2.0.0.tgz").Code, http.StatusOK)
}

func TestChartReleaserDirectDownloadsBeforeAndAfterFailedRefresh(t *testing.T) {
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "download"))
	defer cancel()
	indexRequests, downloadRequests := 0, 0
	serviceBackend := githubclient.NewClient(&http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/repos/acme/charts/contents/index.yaml" {
			indexRequests++
			return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("failed")), Header: make(http.Header)}, nil
		}
		downloadRequests++
		if r.Context().Value(contextKey{}) != "download" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("download context or authentication changed")
		}
		switch r.URL.Path {
		case "/acme/charts/releases/download/v1/nested/unindexed.tgz":
			if r.URL.Host != "github.com" {
				t.Errorf("release host = %q", r.URL.Host)
			}
		case "/repos/acme/charts/contents/packages/nested/unindexed.tgz":
			if r.URL.Query().Get("ref") != "custom" {
				t.Errorf("branch = %s", r.URL)
			}
		default:
			t.Errorf("unexpected download: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("direct bytes")), Header: make(http.Header)}, nil
	})})
	service := startup.New(config.Config{Repositories: []config.Repository{{Name: "pages", Type: config.ChartReleaserType, Owner: "acme", Repo: "charts", Branch: "custom", GitHubToken: "secret"}}}, nil, slog.Default(), serviceBackend, time.Now, context.Background())
	handler := service.Handler
	for _, phase := range []string{"before refresh", "after failed refresh"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "after failed refresh" {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
				assertStatusCode(t, response.Code, http.StatusOK)
				if strings.Contains(response.Body.String(), "unindexed") {
					t.Fatal("failed source published a package")
				}
			}
			for _, invalid := range []string{"/charts/pages/v1%2Funindexed.tgz", "/charts/pages/v1%252Funindexed.tgz", "/charts/pages/v1/nested%2Funindexed.tgz", "/charts/pages/v1/nested%252Funindexed.tgz"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, invalid, nil).WithContext(ctx))
				assertStatusCode(t, response.Code, http.StatusNotFound)
			}
			for _, path := range []string{"/charts/pages/v1/nested/unindexed.tgz", "/charts/pages/package-in-branch/packages/nested/unindexed.tgz"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
				if response.Code != http.StatusOK || response.Body.String() != "direct bytes" || response.Header().Get("Content-Type") != "application/gzip" || response.Header().Get("Content-Disposition") != `attachment; filename="unindexed.tgz"` {
					t.Fatalf("GET %s = %d, %q, %v", path, response.Code, response.Body.String(), response.Header())
				}
			}
		})
	}
	if indexRequests != 1 || downloadRequests != 4 {
		t.Fatalf("upstream index/download requests = %d/%d", indexRequests, downloadRequests)
	}
}
