package app_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"helm-github-releases-proxy/internal/app"
	"helm-github-releases-proxy/internal/config"
	githubclient "helm-github-releases-proxy/internal/github"
	"helm-github-releases-proxy/internal/repository"
	localsource "helm-github-releases-proxy/internal/source/local"
)

func TestAssembledLocalSourceDownloadsOnlyPublishedPaths(t *testing.T) {
	directory := t.TempDir()
	filename := "demo chart-1.2.3.tgz"
	advertisedFilename := "demo%20chart-1.2.3.tgz"
	path := filepath.Join(directory, filename)
	writeLocalChart(t, path, "demo chart", "1.2.3", "published bytes")
	runtime := app.New(localConfig(directory, 60), nil, discardLogger(), &appGitHubBackend{}, fixedClock, context.Background())

	index := request(runtime.Handler, http.MethodGet, "http://index.example/index.yaml")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "charts/local/"+advertisedFilename) {
		t.Fatalf("index response = %d %q", index.Code, index.Body.String())
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	download := request(runtime.Handler, http.MethodGet, "http://download.example/charts/local/"+advertisedFilename)
	if download.Code != http.StatusOK || download.Body.String() != string(want) {
		t.Fatalf("download response = %d, %q", download.Code, download.Body.String())
	}
	if download.Header().Get("Content-Type") != "application/gzip" || download.Header().Get("Content-Disposition") != `attachment; filename="demo chart-1.2.3.tgz"` {
		t.Fatalf("download headers = %v", download.Header())
	}

	unpublished := "demo-2.0.0.tgz"
	writeLocalChart(t, filepath.Join(directory, unpublished), "demo", "2.0.0", "not published")
	for _, target := range []string{
		"http://download.example/charts/local/" + unpublished,
		"http://download.example/charts/local/7/" + advertisedFilename,
	} {
		response := request(runtime.Handler, http.MethodGet, target)
		if response.Code != http.StatusNotFound || response.Body.String() != "chart not found\n" {
			t.Errorf("GET %s = %d %q, want 404", target, response.Code, response.Body.String())
		}
	}

	replacement := filepath.Join(t.TempDir(), filename)
	writeLocalChart(t, replacement, "demo chart", "1.2.3", "replacement")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, path); err != nil {
		t.Fatal(err)
	}
	symlinked := request(runtime.Handler, http.MethodGet, "http://download.example/charts/local/"+advertisedFilename)
	if symlinked.Code != http.StatusNotFound {
		t.Fatalf("symlink replacement status = %d, want 404", symlinked.Code)
	}
}

func TestAssembledLocalDownloadBeforePublicationDoesNotDiscover(t *testing.T) {
	directory := t.TempDir()
	filename := "demo-1.0.0.tgz"
	writeLocalChart(t, filepath.Join(directory, filename), "demo", "1.0.0", "bytes")
	started := make(chan struct{})
	release := make(chan struct{})
	source := localsource.New("local", directory, discardLogger())
	runtime := app.New(localConfig(directory, 60), nil, discardLogger(), &appGitHubBackend{}, fixedClock, context.Background(), app.WithSource(repository.Source{
		Config: repository.SourceConfig{Name: "local", Kind: repository.LocalSource, Path: filepath.Base(directory)},
		Discovery: discoveryFunc(func(ctx context.Context) (repository.Contribution, error) {
			close(started)
			<-release
			return source.Discover(ctx)
		}),
		Packages: source,
	}))
	indexDone := make(chan struct{})
	go func() {
		defer close(indexDone)
		request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	}()
	<-started

	downloadDone := make(chan int, 1)
	go func() {
		downloadDone <- request(runtime.Handler, http.MethodGet, "http://example.test/charts/local/"+filename).Code
	}()
	select {
	case status := <-downloadDone:
		if status != http.StatusNotFound {
			t.Fatalf("download status = %d, want 404", status)
		}
	case <-time.After(time.Second):
		t.Fatal("download waited for local discovery")
	}
	status := request(runtime.Handler, http.MethodGet, "http://example.test/status")
	if !strings.Contains(status.Body.String(), `"status":"unknown"`) || !strings.Contains(status.Body.String(), `"cache_present":false`) {
		t.Fatalf("status after download = %s", status.Body.String())
	}
	close(release)
	<-indexDone
}

func TestAssembledLocalSourceSanitizesUnavailableRelativePath(t *testing.T) {
	configuredPath := filepath.Join("private", "charts")
	runtime := app.New(localConfig(configuredPath, 60), nil, discardLogger(), &appGitHubBackend{}, fixedClock, context.Background())
	index := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if index.Code != http.StatusOK {
		t.Fatalf("index status = %d", index.Code)
	}
	status := request(runtime.Handler, http.MethodGet, "http://example.test/status")
	if strings.Contains(status.Body.String(), "private") || !strings.Contains(status.Body.String(), `"path":"charts"`) || !strings.Contains(status.Body.String(), `"status":"partial"`) {
		t.Fatalf("status did not sanitize configured path: %s", status.Body.String())
	}
}

func TestAssembledLocalPublicationFallbackAndReplacementMoveLookupTogether(t *testing.T) {
	directory := t.TempDir()
	oldFilename := "demo-1.0.0.tgz"
	newFilename := "demo-2.0.0.tgz"
	writeLocalChart(t, filepath.Join(directory, oldFilename), "demo", "1.0.0", "old")
	backend := &appGitHubBackend{releases: []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: "remote-1.0.0.tgz"}}}}}
	cfg := config.Config{CacheTTLSeconds: 0, Sources: []config.SourceConfig{
		{Name: "remote", Kind: config.GitHubReleasesKind, Owner: "acme", Repo: "charts"},
		{Name: "local", Kind: config.LocalSourceKind, Path: directory},
	}}
	runtime := app.New(cfg, nil, discardLogger(), backend, fixedClock, context.Background())

	first := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), oldFilename) {
		t.Fatalf("first index = %d %q", first.Code, first.Body.String())
	}
	writeLocalChart(t, filepath.Join(directory, newFilename), "demo", "2.0.0", "new")
	backend.setListError(errors.New("remote unavailable"))
	failed := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if failed.Body.String() != first.Body.String() {
		t.Fatalf("failed refresh replaced publication:\n%s\nwant:\n%s", failed.Body.String(), first.Body.String())
	}
	assertDownloadStatus(t, runtime, oldFilename, http.StatusOK)
	assertDownloadStatus(t, runtime, newFilename, http.StatusNotFound)

	if err := os.Remove(filepath.Join(directory, oldFilename)); err != nil {
		t.Fatal(err)
	}
	backend.setListError(nil)
	replaced := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
	if replaced.Code != http.StatusOK || strings.Contains(replaced.Body.String(), oldFilename) || !strings.Contains(replaced.Body.String(), newFilename) {
		t.Fatalf("replacement index = %d %q", replaced.Code, replaced.Body.String())
	}
	assertDownloadStatus(t, runtime, oldFilename, http.StatusNotFound)
	assertDownloadStatus(t, runtime, newFilename, http.StatusOK)
}

func TestAssembledLocalSourceCombinesWithConfiguredGitHubModes(t *testing.T) {
	for _, mode := range []config.SourceKind{config.GitHubReleasesKind, config.ChartReleaserKind} {
		t.Run(string(mode), func(t *testing.T) {
			directory := t.TempDir()
			localFilename := "demo-1.2.3.tgz"
			writeLocalChart(t, filepath.Join(directory, localFilename), "demo", "1.2.3", "local")
			backend := &appGitHubBackend{
				releases: []githubclient.Release{{Assets: []githubclient.Asset{{ID: 7, Name: localFilename, Digest: "sha256:remote-digest"}}}},
				branch: `apiVersion: v1
entries:
  demo:
    - apiVersion: v2
      name: demo
      version: 1.2.3
      description: remote metadata
      digest: remote-digest
      urls: [https://github.com/acme/charts/releases/download/v1.2.3/demo-1.2.3.tgz]
`,
			}
			remote := config.SourceConfig{Name: string(mode), Kind: mode, Owner: "acme", Repo: "charts", Branch: "gh-pages"}
			cfg := config.Config{CacheTTLSeconds: 60, Sources: []config.SourceConfig{remote, {Name: "local", Kind: config.LocalSourceKind, Path: directory}}}
			runtime := app.New(cfg, nil, discardLogger(), backend, fixedClock, context.Background())
			index := request(runtime.Handler, http.MethodGet, "http://example.test/index.yaml")
			if index.Code != http.StatusOK {
				t.Fatalf("index status = %d: %s", index.Code, index.Body.String())
			}
			var document struct {
				Entries map[string][]struct {
					Description string   `yaml:"description"`
					Digest      string   `yaml:"digest"`
					URLs        []string `yaml:"urls"`
				} `yaml:"entries"`
			}
			if err := yaml.Unmarshal(index.Body.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			entry := document.Entries["demo"][0]
			if entry.Digest != "remote-digest" || len(entry.URLs) != 2 || entry.URLs[1] != "charts/local/"+localFilename {
				t.Fatalf("combined entry = %#v", entry)
			}
			if mode == config.ChartReleaserKind && entry.Description != "remote metadata" {
				t.Fatalf("metadata precedence = %#v", entry)
			}
			for _, advertisedPath := range entry.URLs {
				response := request(runtime.Handler, http.MethodGet, "http://example.test/"+advertisedPath)
				if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/gzip" {
					t.Fatalf("GET %s = %d, headers %v", advertisedPath, response.Code, response.Header())
				}
			}
		})
	}
}

type appGitHubBackend struct {
	mu        sync.Mutex
	releases  []githubclient.Release
	listError error
	branch    string
}

func (backend *appGitHubBackend) ListReleases(context.Context, string, string, string) ([]githubclient.Release, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.releases, backend.listError
}

func (backend *appGitHubBackend) DownloadAsset(context.Context, string, string, int64, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("release bytes")), nil
}

func (backend *appGitHubBackend) FetchBranchFile(_ context.Context, _, _, _, path, _ string) (io.ReadCloser, error) {
	if path == "index.yaml" {
		return io.NopCloser(strings.NewReader(backend.branch)), nil
	}
	return io.NopCloser(strings.NewReader("branch bytes")), nil
}

func (backend *appGitHubBackend) DownloadRelease(context.Context, string, string, string, string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("release bytes")), nil
}

func (backend *appGitHubBackend) setListError(err error) {
	backend.mu.Lock()
	backend.listError = err
	backend.mu.Unlock()
}

func localConfig(directory string, ttl int) config.Config {
	return config.Config{CacheTTLSeconds: ttl, Sources: []config.SourceConfig{{Name: "local", Kind: config.LocalSourceKind, Path: directory}}}
}

func assertDownloadStatus(t *testing.T, runtime *app.Runtime, filename string, status int) {
	t.Helper()
	response := request(runtime.Handler, http.MethodGet, "http://example.test/charts/local/"+filename)
	if response.Code != status {
		t.Fatalf("GET %s = %d %q, want %d", filename, response.Code, response.Body.String(), status)
	}
}

func writeLocalChart(t *testing.T, filename, name, version, payload string) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	metadata := "name: " + name + "\nversion: " + version + "\n"
	for _, entry := range []struct{ name, body string }{{name + "/Chart.yaml", metadata}, {name + "/values.yaml", payload}} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tarWriter, strings.NewReader(entry.body)); err != nil {
			t.Fatal(err)
		}
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
	modified := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filename, modified, modified); err != nil {
		t.Fatal(err)
	}
}
