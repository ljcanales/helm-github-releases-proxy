package sources_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"helm-github-releases-proxy/internal/application"
	"helm-github-releases-proxy/internal/config"
	"helm-github-releases-proxy/internal/sources"
)

type chartReleaserTransport struct {
	index                    string
	path, tag, branch, token string
	ctx                      context.Context
}

func (f *chartReleaserTransport) FetchBranchFile(ctx context.Context, _, _, branch, path, token string) (io.ReadCloser, error) {
	f.ctx, f.branch, f.path, f.token = ctx, branch, path, token
	if path == "index.yaml" {
		return io.NopCloser(strings.NewReader(f.index)), nil
	}
	return io.NopCloser(strings.NewReader("branch bytes")), nil
}
func (f *chartReleaserTransport) DownloadRelease(ctx context.Context, _, _, tag, filename, token string) (io.ReadCloser, error) {
	f.ctx, f.tag, f.path, f.token = ctx, tag, filename, token
	return io.NopCloser(strings.NewReader("release bytes")), nil
}
func TestChartReleaserDiscoveryRejectsWholeContribution(t *testing.T) {
	for _, invalid := range []string{"version: 2.0.0\n      urls: [https://other.test/chart.tgz]", "urls: []"} {
		backend := &chartReleaserTransport{index: "entries:\n  demo:\n    - version: 1.0.0\n      urls: [demo-1.0.0.tgz]\n    - " + invalid + "\n"}
		source := sources.NewChartReleaser(config.Repository{Name: "pages", Owner: "acme", Repo: "charts", Branch: "custom"}, backend)
		contribution, err := source.Discover(context.Background())
		if err == nil || len(contribution.ChartVersions) != 0 || contribution.IndexedCount != 0 {
			t.Fatalf("contribution = %#v, error = %v", contribution, err)
		}
	}
}
func TestChartReleaserOpensValidatedDirectPackages(t *testing.T) {
	backend := &chartReleaserTransport{}
	source := sources.NewChartReleaser(config.Repository{Name: "pages", Owner: "acme", Repo: "charts", Branch: "custom", GitHubToken: "secret"}, backend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, tc := range []struct{ key, tag, path, body string }{
		{"v1/nested/demo.tgz", "v1/nested", "demo.tgz", "release bytes"},
		{"package-in-branch/packages/nested/demo.tgz", "", "packages/nested/demo.tgz", "branch bytes"},
	} {
		backend.tag = ""
		pkg, err := source.OpenPackage(ctx, application.PackageReference{Source: "pages", Key: tc.key})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(pkg.Body)
		pkg.Body.Close()
		if err != nil || string(body) != tc.body || pkg.Filename != "demo.tgz" || backend.path != tc.path || backend.tag != tc.tag || backend.ctx != ctx || backend.token != "secret" {
			t.Fatalf("package = %#v, body = %q, transport = %#v", pkg, body, backend)
		}
	}
	if backend.branch != "custom" {
		t.Fatalf("branch = %q", backend.branch)
	}
	for _, key := range []string{"package-in-branch/%252e%252e/demo.tgz", "tag/demo.zip", "tag/../demo.tgz", "tag/demo%ZZ.tgz", "tag/foo\\demo.tgz", "tag//demo.tgz"} {
		_, err := source.OpenPackage(ctx, application.PackageReference{Source: "pages", Key: key})
		if !errors.Is(err, application.ErrPackageNotFound) {
			t.Fatalf("key %q: %v", key, err)
		}
	}
	_, err := source.OpenPackage(ctx, application.PackageReference{Source: "other", Key: "tag/demo.tgz"})
	if !errors.Is(err, application.ErrPackageNotFound) {
		t.Fatal(err)
	}
}

func TestChartReleaserDiscoveryMergesVersionsAndEscapesPackageURLs(t *testing.T) {
	backend := &chartReleaserTransport{index: `entries:
  demo:
    - version: 1.0.0
      description: first metadata
      unknown: discarded
      urls:
        - https://github.com/acme/charts/releases/download/tag+build/demo%20chart.tgz
        - packages/demo%20chart.tgz
    - version: 1.0.0
      description: later metadata
      urls: [packages/demo%20chart.tgz]
`}
	source := sources.NewChartReleaser(config.Repository{Name: "pages", Owner: "acme", Repo: "charts", Branch: "custom", GitHubToken: "secret"}, backend)
	result, err := source.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.IndexedCount != 1 || result.SkippedCount != 0 || len(result.ChartVersions) != 1 {
		t.Fatalf("contribution = %#v", result)
	}
	chart := result.ChartVersions[0]
	if chart.Version.Name != "demo" || chart.Version.APIVersion != "v2" || chart.Version.Description != "first metadata" || len(chart.Packages) != 2 {
		t.Fatalf("chart = %#v", chart)
	}
	for i, want := range []string{"charts/pages/tag+build/demo%20chart.tgz", "charts/pages/package-in-branch/packages/demo%20chart.tgz"} {
		if chart.Version.URLs[i] != want || chart.Packages[i].URL != want || chart.Packages[i].Reference.Source != "pages" {
			t.Fatalf("package %d = %#v", i, chart.Packages[i])
		}
		pkg, err := source.OpenPackage(context.Background(), chart.Packages[i].Reference)
		if err != nil {
			t.Fatal(err)
		}
		pkg.Body.Close()
		if backend.path != "demo chart.tgz" && backend.path != "packages/demo chart.tgz" {
			t.Fatalf("path = %q", backend.path)
		}
	}
}
