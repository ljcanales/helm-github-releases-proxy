package sources

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"helm-github-releases-proxy/internal/application"
	"helm-github-releases-proxy/internal/catalog"
	"helm-github-releases-proxy/internal/config"
)

// ChartReleaserBackend supplies branch files and release packages without
// requiring release-asset listing or indexed membership.
type ChartReleaserBackend interface {
	FetchBranchFile(context.Context, string, string, string, string, string) (io.ReadCloser, error)
	DownloadRelease(context.Context, string, string, string, string, string) (io.ReadCloser, error)
}

// ChartReleaser interprets its branch index and permits validated direct downloads.
// It stores configuration and transport, never refresh or publication state.
type ChartReleaser struct {
	repository config.Repository
	backend    ChartReleaserBackend
}

var _ application.ChartDiscovery = (*ChartReleaser)(nil)
var _ application.PackageOpener = (*ChartReleaser)(nil)

func NewChartReleaser(repository config.Repository, backend ChartReleaserBackend) *ChartReleaser {
	return &ChartReleaser{repository: repository, backend: backend}
}

func (s *ChartReleaser) Discover(ctx context.Context) (application.SourceContribution, error) {
	r := s.repository
	file, err := s.backend.FetchBranchFile(ctx, r.Owner, r.Repo, r.Branch, "index.yaml", r.GitHubToken)
	if err != nil {
		return application.SourceContribution{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return application.SourceContribution{}, err
	}
	entries := make(catalog.AggregateIndex)
	if err := addChartReleaserEntries(entries, r.Name, r.Owner, r.Repo, data); err != nil {
		return application.SourceContribution{}, err
	}
	result := application.SourceContribution{}
	for _, versions := range entries {
		for _, version := range versions {
			chart := application.DiscoveredChartVersion{Version: *version}
			for _, chartURL := range version.URLs {
				chart.Packages = append(chart.Packages, application.DiscoveredPackage{URL: chartURL, Reference: application.PackageReference{Source: r.Name, Key: strings.TrimPrefix(chartURL, "charts/"+r.Name+"/")}})
			}
			result.ChartVersions = append(result.ChartVersions, chart)
			result.IndexedCount++
		}
	}
	return result, nil
}

func (s *ChartReleaser) OpenPackage(ctx context.Context, reference application.PackageReference) (application.ChartPackage, error) {
	parts, safe := chartReleaserPathParts(reference.Key)
	if reference.Source != s.repository.Name || !safe {
		return application.ChartPackage{}, application.ErrPackageNotFound
	}
	r := s.repository
	filename := parts[len(parts)-1]
	var body io.ReadCloser
	var err error
	if parts[0] == "package-in-branch" {
		body, err = s.backend.FetchBranchFile(ctx, r.Owner, r.Repo, r.Branch, strings.Join(parts[1:], "/"), r.GitHubToken)
	} else {
		body, err = s.backend.DownloadRelease(ctx, r.Owner, r.Repo, strings.Join(parts[:len(parts)-1], "/"), filename, r.GitHubToken)
	}
	if err != nil {
		return application.ChartPackage{}, err
	}
	return application.ChartPackage{Body: body, Filename: filename}, nil
}

type chartReleaserMaintainer struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
	URL   string `yaml:"url,omitempty"`
}

type chartReleaserIndex struct {
	Entries map[string][]chartReleaserEntry `yaml:"entries"`
}

// chartReleaserEntry is deliberately closed: yaml.v3 ignores unknown fields,
// while these fields define the common aggregate index contract.
type chartReleaserEntry struct {
	APIVersion  string                    `yaml:"apiVersion,omitempty"`
	Name        string                    `yaml:"name,omitempty"`
	Version     string                    `yaml:"version,omitempty"`
	AppVersion  string                    `yaml:"appVersion,omitempty"`
	Description string                    `yaml:"description,omitempty"`
	Home        string                    `yaml:"home,omitempty"`
	Icon        string                    `yaml:"icon,omitempty"`
	Deprecated  *bool                     `yaml:"deprecated,omitempty"`
	Keywords    []string                  `yaml:"keywords,omitempty"`
	Maintainers []chartReleaserMaintainer `yaml:"maintainers,omitempty"`
	Sources     []string                  `yaml:"sources,omitempty"`
	Annotations map[string]string         `yaml:"annotations,omitempty"`
	Created     string                    `yaml:"created,omitempty"`
	Digest      string                    `yaml:"digest,omitempty"`
	URLs        []string                  `yaml:"urls"`
}

func addChartReleaserEntries(entries catalog.AggregateIndex, repository, owner, repo string, data []byte) error {
	var source chartReleaserIndex
	if err := yaml.Unmarshal(data, &source); err != nil {
		return fmt.Errorf("decode chart-releaser index: %w", err)
	}
	if source.Entries == nil {
		return fmt.Errorf("chart-releaser index has no entries")
	}
	for chartName, versions := range source.Entries {
		for _, sourceEntry := range versions {
			if sourceEntry.Name == "" {
				sourceEntry.Name = chartName
			}
			if sourceEntry.Version == "" || len(sourceEntry.URLs) == 0 {
				return fmt.Errorf("chart %q has an incomplete entry", chartName)
			}

			maintainers := make([]catalog.Maintainer, len(sourceEntry.Maintainers))
			for i, m := range sourceEntry.Maintainers {
				maintainers[i] = catalog.Maintainer{Name: m.Name, Email: m.Email, URL: m.URL}
			}
			entry := catalog.ChartVersion{APIVersion: sourceEntry.APIVersion, Name: sourceEntry.Name, Version: sourceEntry.Version, AppVersion: sourceEntry.AppVersion, Description: sourceEntry.Description, Home: sourceEntry.Home, Icon: sourceEntry.Icon, Deprecated: sourceEntry.Deprecated, Keywords: sourceEntry.Keywords, Maintainers: maintainers, Sources: sourceEntry.Sources, Annotations: sourceEntry.Annotations, Created: sourceEntry.Created, Digest: sourceEntry.Digest}
			if entry.APIVersion == "" {
				entry.APIVersion = "v2"
			}
			for _, rawURL := range sourceEntry.URLs {
				rewritten, err := rewriteChartReleaserURL(rawURL, owner, repo, repository)
				if err != nil {
					return fmt.Errorf("chart %q: %w", chartName, err)
				}
				entry.URLs = append(entry.URLs, rewritten)
			}
			entries.Add(entry)
		}
	}
	return nil
}

func rewriteChartReleaserURL(rawURL, owner, repo, repository string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || rawURL == "" || parsed.Query().Encode() != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("unsupported chart URL %q", rawURL)
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if parsed.Scheme != "https" || parsed.Host != "github.com" || len(parts) < 6 || parts[0] != owner || parts[1] != repo || parts[2] != "releases" || parts[3] != "download" {
			return "", fmt.Errorf("unsupported chart URL %q", rawURL)
		}
		return "charts/" + repository + "/" + escapePath(parts[4:]), nil
	}
	if parsed.Path == "" || strings.HasPrefix(parsed.Path, "/") || !strings.HasSuffix(parsed.Path, ".tgz") {
		return "", fmt.Errorf("unsupported chart URL %q", rawURL)
	}
	parts := strings.Split(parsed.Path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsupported chart URL %q", rawURL)
		}
	}
	return "charts/" + repository + "/package-in-branch/" + escapePath(parts), nil
}

func escapePath(parts []string) string {
	encoded := make([]string, len(parts))
	for i, part := range parts {
		encoded[i] = url.PathEscape(part)
	}
	return strings.Join(encoded, "/")
}

// chartReleaserPathParts decodes and validates a direct package path.
func chartReleaserPathParts(rawPath string) ([]string, bool) {
	decoded := rawPath
	for {
		next, err := url.PathUnescape(decoded)
		if err != nil {
			return nil, false
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	parts := strings.Split(decoded, "/")
	if len(parts) < 2 || !strings.HasSuffix(parts[len(parts)-1], ".tgz") {
		return nil, false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\\`) {
			return nil, false
		}
	}
	return parts, true
}
