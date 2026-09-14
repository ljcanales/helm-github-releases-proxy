package httpapi_test

import (
	"context"
	githubclient "helm-github-releases-proxy/internal/github"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helm-github-releases-proxy/internal/application"
	"helm-github-releases-proxy/internal/config"
	"helm-github-releases-proxy/internal/sources"
	"helm-github-releases-proxy/internal/startup"
)

func TestLocalSourceDiscoversAndOpensChartPackages(t *testing.T) {
	directory := t.TempDir()
	filename := "demo-v1.2.3.tgz"
	path := filepath.Join(directory, filename)
	writeTestChart(t, path, "demo", "1.2.3", "chart bytes")
	if err := os.WriteFile(filepath.Join(directory, "bad.tgz"), []byte("bad"), 0644); err != nil {
		t.Fatal(err)
	}
	source := sources.NewLocal("local", directory, slog.Default())
	var discovery application.ChartDiscovery = source
	contribution, err := discovery.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contribution.IndexedCount != 1 || contribution.SkippedCount != 1 || len(contribution.ChartVersions) != 1 {
		t.Fatalf("contribution = %#v", contribution)
	}
	chart := contribution.ChartVersions[0]
	if chart.Version.Name != "demo" || chart.Version.Version != "1.2.3" || chart.Version.Digest == "" || len(chart.Packages) != 1 || chart.Packages[0].URL != "charts/local/"+filename {
		t.Fatalf("chart = %#v", chart)
	}
	var opener application.PackageOpener = source
	pkg, err := opener.OpenPackage(context.Background(), chart.Packages[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	defer pkg.Body.Close()
	got, err := io.ReadAll(pkg.Body)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || pkg.Filename != filename {
		t.Fatal("opened package differs from discovered archive")
	}
	for _, ref := range []application.PackageReference{{Source: "other", Key: filename}, {Source: "local", Key: "../" + filename}, {Source: "local", Key: path}} {
		if pkg, err := opener.OpenPackage(context.Background(), ref); err == nil {
			pkg.Body.Close()
			t.Fatalf("opened invalid reference: %#v", ref)
		}
	}
}

func TestHTTPServiceRejectsIndexedLocalPackageReplacedBySymlink(t *testing.T) {
	directory := t.TempDir()
	filename := "demo-1.2.3.tgz"
	path := filepath.Join(directory, filename)
	writeTestChart(t, path, "demo", "1.2.3", "chart bytes")
	service := startup.New(config.Config{CacheTTLSeconds: 60, Repositories: []config.Repository{{Name: "local", Type: config.LocalDirectoryType, Path: directory}}}, nil, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	handler := service.Handler
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, index.Code, http.StatusOK)
	target := filepath.Join(t.TempDir(), filename)
	writeTestChart(t, target, "demo", "1.2.3", "replacement")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	download := httptest.NewRecorder()
	handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/charts/local/"+filename, nil))
	assertStatusCode(t, download.Code, http.StatusNotFound)
	if download.Body.String() != "chart not found\n" {
		t.Fatalf("body = %q", download.Body.String())
	}
}

func TestHTTPServiceRetainsLocalPackageLookupOnAggregateFailure(t *testing.T) {
	directory := t.TempDir()
	unavailable := t.TempDir()
	oldFilename := "demo-1.0.0.tgz"
	newFilename := "demo-2.0.0.tgz"
	writeTestChart(t, filepath.Join(directory, oldFilename), "demo", "1.0.0", "old archive")
	service := startup.New(config.Config{Repositories: []config.Repository{
		{Name: "local", Type: config.LocalDirectoryType, Path: directory},
		{Name: "other", Type: config.LocalDirectoryType, Path: unavailable},
	}}, nil, slog.Default(), githubclient.NewClient(nil), time.Now, context.Background())
	handler := service.Handler
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, first.Code, http.StatusOK)
	writeTestChart(t, filepath.Join(directory, newFilename), "demo", "2.0.0", "new archive")
	if err := os.Remove(unavailable); err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/index.yaml", nil))
	assertStatusCode(t, second.Code, http.StatusOK)
	if second.Body.String() != first.Body.String() {
		t.Fatal("failed refresh replaced the aggregate")
	}
	for _, tc := range []struct {
		filename string
		status   int
	}{{oldFilename, http.StatusOK}, {newFilename, http.StatusNotFound}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/charts/local/"+tc.filename, nil))
		assertStatusCode(t, response.Code, tc.status)
	}
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(status.Body.String(), `"indexed_count":2`) || !strings.Contains(status.Body.String(), `"status":"stale"`) {
		t.Fatalf("status = %s", status.Body.String())
	}
}
