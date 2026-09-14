package sources

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"helm-github-releases-proxy/internal/application"
	"helm-github-releases-proxy/internal/catalog"
	"helm-github-releases-proxy/internal/config"
	githubclient "helm-github-releases-proxy/internal/github"
)

// GitHubReleaseBackend is the transport used by a release chart source.
type GitHubReleaseBackend interface {
	ListReleases(context.Context, string, string, string) ([]githubclient.Release, error)
	DownloadAsset(context.Context, string, string, int64, string) (io.ReadCloser, error)
}

// GitHubReleases holds configuration and transport, never published lookup state.
// The caller must verify published asset-ID and filename membership before opening.
type GitHubReleases struct {
	repository config.Repository
	backend    GitHubReleaseBackend
	logger     *slog.Logger
}

var _ application.ChartDiscovery = (*GitHubReleases)(nil)
var _ application.PackageOpener = (*GitHubReleases)(nil)

func NewGitHubReleases(repository config.Repository, backend GitHubReleaseBackend, logger *slog.Logger) *GitHubReleases {
	return &GitHubReleases{repository: repository, backend: backend, logger: logger}
}

func (s *GitHubReleases) Discover(ctx context.Context) (application.SourceContribution, error) {
	r := s.repository
	releases, err := s.backend.ListReleases(ctx, r.Owner, r.Repo, r.GitHubToken)
	if err != nil {
		return application.SourceContribution{}, err
	}
	result := application.SourceContribution{}
	for _, release := range releases {
		for _, asset := range release.Assets {
			if !strings.HasSuffix(asset.Name, ".tgz") {
				result.SkippedCount++
				s.logger.Debug("skipping non-chart GitHub release asset", "repository", r.Name, "filename", asset.Name)
				continue
			}
			name, version, ok := parseGitHubChartFilename(asset.Name)
			if !ok {
				result.SkippedCount++
				s.logger.Debug("skipping invalid GitHub chart filename", "repository", r.Name, "filename", asset.Name)
				continue
			}
			key := strconv.FormatInt(asset.ID, 10) + "/" + asset.Name
			chartURL := "charts/" + r.Name + "/" + key
			chart := catalog.ChartVersion{APIVersion: "v2", Name: name, Version: version, URLs: []string{chartURL}}
			if release.PublishedAt != nil {
				chart.Created = release.PublishedAt.UTC().Format(time.RFC3339Nano)
			}
			if strings.HasPrefix(asset.Digest, "sha256:") {
				chart.Digest = strings.TrimPrefix(asset.Digest, "sha256:")
			}
			result.ChartVersions = append(result.ChartVersions, application.DiscoveredChartVersion{
				Version:  chart,
				Packages: []application.DiscoveredPackage{{URL: chartURL, Reference: application.PackageReference{Source: r.Name, Key: key}}},
			})
			result.IndexedCount++
		}
	}
	if result.SkippedCount > 0 {
		s.logger.Debug("skipped GitHub release assets", "repository", r.Name, "count", result.SkippedCount)
	}
	return result, nil
}

func (s *GitHubReleases) OpenPackage(ctx context.Context, reference application.PackageReference) (application.ChartPackage, error) {
	id, filename, ok := strings.Cut(reference.Key, "/")
	assetID, err := strconv.ParseInt(id, 10, 64)
	if reference.Source != s.repository.Name || !ok || err != nil || assetID < 1 || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) || filename == "." || filename == ".." {
		return application.ChartPackage{}, os.ErrNotExist
	}
	r := s.repository
	body, err := s.backend.DownloadAsset(ctx, r.Owner, r.Repo, assetID, r.GitHubToken)
	if err != nil {
		return application.ChartPackage{}, err
	}
	return application.ChartPackage{Body: body, Filename: filename}, nil
}

var githubChartFilenamePattern = regexp.MustCompile(`^(.+)-v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\.tgz$`)

func parseGitHubChartFilename(filename string) (string, string, bool) {
	match := githubChartFilenamePattern.FindStringSubmatch(filename)
	if match == nil || match[1] == "" {
		return "", "", false
	}
	return match[1], match[2], true
}
