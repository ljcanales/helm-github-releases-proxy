package githubreleases_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	githubclient "helm-github-releases-proxy/internal/github"
	"helm-github-releases-proxy/internal/repository"
	"helm-github-releases-proxy/internal/source/githubreleases"
)

func TestSourceDiscoversEligibleReleaseAssets(t *testing.T) {
	published := time.Date(2026, 8, 30, 12, 0, 0, 123, time.FixedZone("release", -4*60*60))
	backend := &releaseBackend{releases: []githubclient.Release{{PublishedAt: &published, Assets: []githubclient.Asset{
		{ID: 7, Name: "demo-chart-v1.2.3.tgz", Digest: "sha256:abc123"},
		{ID: 8, Name: "demo-chart-2.0.0-rc.1+build.4.tgz", Digest: "sha512:ignored"},
		{ID: 9, Name: "notes.txt"},
		{ID: 10, Name: "invalid.tgz"},
	}}}}
	source := githubreleases.New("releases", "acme", "charts", backend, discardLogger(), githubreleases.WithToken("secret"))

	contribution, err := source.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contribution.IndexedCount != 2 || contribution.SkippedCount != 2 || len(contribution.ChartVersions) != 2 {
		t.Fatalf("contribution = %#v", contribution)
	}
	first := contribution.ChartVersions[0]
	if first.Version.Name != "demo-chart" || first.Version.Version != "1.2.3" || first.Version.Created != "2026-08-30T16:00:00.000000123Z" || first.Version.Digest != "abc123" {
		t.Fatalf("first version = %#v", first.Version)
	}
	if len(first.Packages) != 1 || first.Packages[0].AdvertisedPath != "charts/releases/7/demo-chart-v1.2.3.tgz" {
		t.Fatalf("first package = %#v", first.Packages)
	}
	second := contribution.ChartVersions[1]
	if second.Version.Name != "demo-chart" || second.Version.Version != "2.0.0-rc.1+build.4" || second.Version.Digest != "" {
		t.Fatalf("second version = %#v", second.Version)
	}
	if backend.listOwner != "acme" || backend.listRepo != "charts" || backend.listToken != "secret" {
		t.Fatalf("list request = owner %q, repo %q, token %q", backend.listOwner, backend.listRepo, backend.listToken)
	}

	pkg, err := source.OpenPackage(context.Background(), first.Packages[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	defer pkg.Body.Close()
	body, err := io.ReadAll(pkg.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "asset bytes" || pkg.Filename != "demo-chart-v1.2.3.tgz" {
		t.Fatalf("package = filename %q, body %q", pkg.Filename, body)
	}
	if backend.downloadOwner != "acme" || backend.downloadRepo != "charts" || backend.downloadID != 7 || backend.downloadToken != "secret" {
		t.Fatalf("download request = owner %q, repo %q, id %d, token %q", backend.downloadOwner, backend.downloadRepo, backend.downloadID, backend.downloadToken)
	}
}

func TestSourceRejectsInvalidReferencesBeforeDownloading(t *testing.T) {
	backend := &releaseBackend{}
	source := githubreleases.New("releases", "acme", "charts", backend, discardLogger())
	for _, reference := range []repository.PackageReference{
		{Source: "other", Key: "7/demo-1.0.0.tgz"},
		{Source: "releases", Key: ""},
		{Source: "releases", Key: "0/demo-1.0.0.tgz"},
		{Source: "releases", Key: "not-a-number/demo-1.0.0.tgz"},
		{Source: "releases", Key: "7/"},
		{Source: "releases", Key: "7/nested/demo-1.0.0.tgz"},
		{Source: "releases", Key: "7/../demo-1.0.0.tgz"},
	} {
		pkg, err := source.OpenPackage(context.Background(), reference)
		if pkg.Body != nil {
			pkg.Body.Close()
		}
		if !errors.Is(err, repository.ErrPackageNotFound) {
			t.Errorf("OpenPackage(%#v) error = %v, want package not found", reference, err)
		}
	}
	if backend.downloadCalls != 0 {
		t.Fatalf("download calls = %d, want 0", backend.downloadCalls)
	}
}

func TestSourcePreservesDiscoveryAndDownloadErrors(t *testing.T) {
	discoveryErr := errors.New("release API unavailable")
	backend := &releaseBackend{listErr: discoveryErr}
	source := githubreleases.New("releases", "acme", "charts", backend, discardLogger())
	if _, err := source.Discover(context.Background()); !errors.Is(err, discoveryErr) {
		t.Fatalf("Discover error = %v", err)
	}

	downloadErr := errors.New("asset API unavailable")
	backend.listErr = nil
	backend.downloadErr = downloadErr
	if _, err := source.OpenPackage(context.Background(), repository.PackageReference{Source: "releases", Key: "7/demo-1.0.0.tgz"}); !errors.Is(err, downloadErr) {
		t.Fatalf("OpenPackage error = %v", err)
	}
}

type releaseBackend struct {
	releases                                   []githubclient.Release
	listErr, downloadErr                       error
	listOwner, listRepo, listToken             string
	downloadOwner, downloadRepo, downloadToken string
	downloadID                                 int64
	downloadCalls                              int
}

func (backend *releaseBackend) ListReleases(_ context.Context, owner, repo, token string) ([]githubclient.Release, error) {
	backend.listOwner, backend.listRepo, backend.listToken = owner, repo, token
	return backend.releases, backend.listErr
}

func (backend *releaseBackend) DownloadAsset(_ context.Context, owner, repo string, assetID int64, token string) (io.ReadCloser, error) {
	backend.downloadCalls++
	backend.downloadOwner, backend.downloadRepo, backend.downloadID, backend.downloadToken = owner, repo, assetID, token
	if backend.downloadErr != nil {
		return nil, backend.downloadErr
	}
	return io.NopCloser(strings.NewReader("asset bytes")), nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
