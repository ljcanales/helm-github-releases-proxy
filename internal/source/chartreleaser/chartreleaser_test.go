package chartreleaser_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"helm-github-releases-proxy/internal/repository"
	"helm-github-releases-proxy/internal/source/chartreleaser"
)

type transport struct {
	index                     string
	branch, path, tag, file   string
	branchCalls, releaseCalls int
}

func (backend *transport) FetchBranchFile(_ context.Context, _, _, branch, path, _ string) (io.ReadCloser, error) {
	backend.branchCalls++
	backend.branch, backend.path = branch, path
	if path == "index.yaml" {
		return io.NopCloser(strings.NewReader(backend.index)), nil
	}
	return io.NopCloser(strings.NewReader("branch bytes")), nil
}

func (backend *transport) DownloadRelease(_ context.Context, _, _, tag, filename, _ string) (io.ReadCloser, error) {
	backend.releaseCalls++
	backend.tag, backend.file = tag, filename
	return io.NopCloser(strings.NewReader("release bytes")), nil
}

var _ chartreleaser.Backend = (*transport)(nil)

func TestSourceDiscoversPublishedReleaseAndBranchReferences(t *testing.T) {
	backend := &transport{index: `entries:
  demo:
    - version: 1.0.0
      description: first metadata
      maintainers:
        - name: Maintainer
          email: maintainer@example.test
      unknown: discarded
      urls:
        - https://github.com/acme/charts/releases/download/release+build/nested/demo%20chart.tgz
        - packages/nested/demo%20chart.tgz
    - version: 1.0.0
      description: later metadata
      urls: [packages/nested/demo%20chart.tgz]
`}
	source := chartreleaser.New("pages", "acme", "charts", "custom", backend, chartreleaser.WithToken("secret"))

	contribution, err := source.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contribution.IndexedCount != 1 || contribution.SkippedCount != 0 || len(contribution.ChartVersions) != 1 {
		t.Fatalf("contribution = %#v", contribution)
	}
	version := contribution.ChartVersions[0]
	if version.Version.Name != "demo" || version.Version.APIVersion != "v2" || version.Version.Description != "first metadata" || len(version.Version.Maintainers) != 1 || version.Version.Maintainers[0].Email != "maintainer@example.test" {
		t.Fatalf("version = %#v", version.Version)
	}
	if len(version.Packages) != 2 {
		t.Fatalf("packages = %#v", version.Packages)
	}
	want := []struct {
		path, key string
	}{
		{"charts/pages/release+build/nested/demo%20chart.tgz", "release+build/nested/demo%20chart.tgz"},
		{"charts/pages/package-in-branch/packages/nested/demo%20chart.tgz", "package-in-branch/packages/nested/demo%20chart.tgz"},
	}
	for index, expected := range want {
		pkg := version.Packages[index]
		if pkg.AdvertisedPath != expected.path || pkg.Reference != (repository.PackageReference{Source: "pages", Key: expected.key}) || version.Version.URLs[index] != expected.path {
			t.Fatalf("package %d = %#v, version = %#v", index, pkg, version.Version)
		}
	}

	release, err := source.OpenPackage(context.Background(), version.Packages[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	releaseBody, _ := io.ReadAll(release.Body)
	release.Body.Close()
	if string(releaseBody) != "release bytes" || backend.tag != "release+build/nested" || backend.file != "demo chart.tgz" || backend.releaseCalls != 1 {
		t.Fatalf("release = %q, transport = %#v", releaseBody, backend)
	}

	branch, err := source.OpenPackage(context.Background(), version.Packages[1].Reference)
	if err != nil {
		t.Fatal(err)
	}
	branchBody, _ := io.ReadAll(branch.Body)
	branch.Body.Close()
	if string(branchBody) != "branch bytes" || backend.branch != "custom" || backend.path != "packages/nested/demo chart.tgz" || backend.branchCalls != 2 {
		t.Fatalf("branch = %q, transport = %#v", branchBody, backend)
	}
}

func TestSourceRejectsInvalidContributionWithoutPartialResults(t *testing.T) {
	backend := &transport{index: "entries:\n  demo:\n    - version: 1.0.0\n      urls: [demo-1.0.0.tgz]\n    - version: 2.0.0\n      urls: []\n"}
	source := chartreleaser.New("pages", "acme", "charts", "custom", backend)

	contribution, err := source.Discover(context.Background())
	if err == nil || len(contribution.ChartVersions) != 0 || contribution.IndexedCount != 0 {
		t.Fatalf("contribution = %#v, error = %v", contribution, err)
	}
}

func TestSourceRejectsUnsafeReferencesBeforeRetrieval(t *testing.T) {
	backend := &transport{}
	source := chartreleaser.New("pages", "acme", "charts", "custom", backend)
	for _, reference := range []repository.PackageReference{
		{Source: "other", Key: "tag/demo.tgz"},
		{Source: "pages", Key: "tag/../demo.tgz"},
		{Source: "pages", Key: "tag/%252e%252e/demo.tgz"},
		{Source: "pages", Key: "tag/foo\\demo.tgz"},
		{Source: "pages", Key: "package-in-branch/packages//demo.tgz"},
		{Source: "pages", Key: "tag/demo.zip"},
	} {
		pkg, err := source.OpenPackage(context.Background(), reference)
		if pkg.Body != nil {
			pkg.Body.Close()
		}
		if !errors.Is(err, repository.ErrPackageNotFound) {
			t.Errorf("reference %#v error = %v, want package not found", reference, err)
		}
	}
	if backend.branchCalls != 0 || backend.releaseCalls != 0 {
		t.Fatalf("retrieval calls = branch %d, release %d", backend.branchCalls, backend.releaseCalls)
	}
}
