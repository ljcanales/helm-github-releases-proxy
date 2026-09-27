// Package githubreleases supplies charts attached to GitHub releases.
package githubreleases

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"helm-github-releases-proxy/internal/chart"
	githubclient "helm-github-releases-proxy/internal/github"
	"helm-github-releases-proxy/internal/logging"
	"helm-github-releases-proxy/internal/repository"
)

// Backend is the GitHub transport used by a release source.
type Backend interface {
	ListReleases(context.Context, string, string, string) ([]githubclient.Release, error)
	DownloadAsset(context.Context, string, string, int64, string) (io.ReadCloser, error)
}

type Option func(*Source)

func WithToken(token string) Option { return func(source *Source) { source.token = token } }

// Source discovers and opens chart assets owned by one configured source.
type Source struct {
	name, owner, repo, token string
	backend                  Backend
	logger                   *slog.Logger
}

func New(name, owner, repo string, backend Backend, logger *slog.Logger, options ...Option) *Source {
	source := &Source{name: name, owner: owner, repo: repo, backend: backend, logger: logger}
	for _, option := range options {
		option(source)
	}
	return source
}

func (source *Source) Discover(ctx context.Context) (repository.Contribution, error) {
	events := logging.New(source.logger)
	releases, err := source.backend.ListReleases(githubclient.WithSource(ctx, source.name), source.owner, source.repo, source.token)
	if err != nil {
		return repository.Contribution{}, err
	}
	result := repository.Contribution{}
	for _, release := range releases {
		for _, asset := range release.Assets {
			if !strings.HasSuffix(asset.Name, ".tgz") {
				result.SkippedCount++
				events.Debug(ctx, "skipping non-chart GitHub release asset", "source", source.name, "filename", asset.Name)
				continue
			}
			name, version, ok := parseChartFilename(asset.Name)
			if !ok {
				result.SkippedCount++
				events.Debug(ctx, "skipping invalid GitHub chart filename", "source", source.name, "filename", asset.Name)
				continue
			}
			key := strconv.FormatInt(asset.ID, 10) + "/" + asset.Name
			advertisedPath := "charts/" + source.name + "/" + key
			metadata := chart.Version{APIVersion: "v2", Name: name, Version: version}
			if release.PublishedAt != nil {
				metadata.Created = release.PublishedAt.UTC().Format(time.RFC3339Nano)
			}
			if strings.HasPrefix(asset.Digest, "sha256:") {
				metadata.Digest = strings.TrimPrefix(asset.Digest, "sha256:")
			}
			result.ChartVersions = append(result.ChartVersions, repository.DiscoveredChartVersion{
				Version: metadata,
				Packages: []repository.DiscoveredPackage{{
					AdvertisedPath: advertisedPath,
					Reference:      repository.PackageReference{Source: source.name, Key: key},
				}},
			})
			result.IndexedCount++
		}
	}
	if result.SkippedCount > 0 {
		events.Debug(ctx, "skipped GitHub release assets", "source", source.name, "count", result.SkippedCount)
	}
	return result, nil
}

// OpenPackage validates source ownership and the opaque asset reference.
// Published path membership is checked by repository before this method is called.
func (source *Source) OpenPackage(ctx context.Context, reference repository.PackageReference) (repository.ChartPackage, error) {
	id, filename, ok := strings.Cut(reference.Key, "/")
	assetID, err := strconv.ParseInt(id, 10, 64)
	if reference.Source != source.name || !ok || err != nil || assetID < 1 || strconv.FormatInt(assetID, 10) != id || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) || filename == "." || filename == ".." {
		return repository.ChartPackage{}, repository.ErrPackageNotFound
	}
	body, err := source.backend.DownloadAsset(githubclient.WithSource(ctx, source.name), source.owner, source.repo, assetID, source.token)
	if err != nil {
		return repository.ChartPackage{}, err
	}
	return repository.ChartPackage{Body: body, Filename: filename}, nil
}

var chartFilenamePattern = regexp.MustCompile(`^(.+)-v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\.tgz$`)

func parseChartFilename(filename string) (string, string, bool) {
	match := chartFilenamePattern.FindStringSubmatch(filename)
	if match == nil || match[1] == "" {
		return "", "", false
	}
	return match[1], match[2], true
}

var _ repository.Discovery = (*Source)(nil)
var _ repository.PackageOpener = (*Source)(nil)
