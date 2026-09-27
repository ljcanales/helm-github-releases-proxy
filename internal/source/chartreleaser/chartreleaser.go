// Package chartreleaser supplies charts published by chart-releaser.
package chartreleaser

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"helm-github-releases-proxy/internal/chart"
	"helm-github-releases-proxy/internal/repository"
)

// Backend is the GitHub transport used by a chart-releaser source.
type Backend interface {
	FetchBranchFile(context.Context, string, string, string, string, string) (io.ReadCloser, error)
	DownloadRelease(context.Context, string, string, string, string, string) (io.ReadCloser, error)
}

type Option func(*Source)

func WithToken(token string) Option { return func(source *Source) { source.token = token } }

// Source discovers chart-releaser's published index and opens only references
// that the repository has already matched to an advertised package path.
type Source struct {
	name, owner, repo, branch, token string
	backend                          Backend
}

func New(name, owner, repo, branch string, backend Backend, options ...Option) *Source {
	source := &Source{name: name, owner: owner, repo: repo, branch: branch, backend: backend}
	for _, option := range options {
		option(source)
	}
	return source
}

func (source *Source) Discover(ctx context.Context) (repository.Contribution, error) {
	file, err := source.backend.FetchBranchFile(ctx, source.owner, source.repo, source.branch, "index.yaml", source.token)
	if err != nil {
		return repository.Contribution{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return repository.Contribution{}, err
	}

	var index chartReleaserIndex
	if err := yaml.Unmarshal(data, &index); err != nil {
		return repository.Contribution{}, fmt.Errorf("decode chart-releaser index: %w", err)
	}
	if index.Entries == nil {
		return repository.Contribution{}, fmt.Errorf("chart-releaser index has no entries")
	}

	aggregate := make(chart.Aggregate)
	references := make(map[string]repository.PackageReference)
	for chartName, entries := range index.Entries {
		for _, sourceEntry := range entries {
			if sourceEntry.Name == "" {
				sourceEntry.Name = chartName
			}
			if sourceEntry.Version == "" || len(sourceEntry.URLs) == 0 {
				return repository.Contribution{}, fmt.Errorf("chart %q has an incomplete entry", chartName)
			}

			maintainers := make([]chart.Maintainer, len(sourceEntry.Maintainers))
			for index, maintainer := range sourceEntry.Maintainers {
				maintainers[index] = chart.Maintainer{Name: maintainer.Name, Email: maintainer.Email, URL: maintainer.URL}
			}
			version := chart.Version{
				APIVersion:  sourceEntry.APIVersion,
				Name:        sourceEntry.Name,
				Version:     sourceEntry.Version,
				AppVersion:  sourceEntry.AppVersion,
				Description: sourceEntry.Description,
				Home:        sourceEntry.Home,
				Icon:        sourceEntry.Icon,
				Deprecated:  sourceEntry.Deprecated,
				Keywords:    sourceEntry.Keywords,
				Maintainers: maintainers,
				Sources:     sourceEntry.Sources,
				Annotations: sourceEntry.Annotations,
				Created:     sourceEntry.Created,
				Digest:      sourceEntry.Digest,
			}
			if version.APIVersion == "" {
				version.APIVersion = "v2"
			}
			for _, rawURL := range sourceEntry.URLs {
				advertisedPath, key, err := rewriteURL(rawURL, source.owner, source.repo, source.name)
				if err != nil {
					return repository.Contribution{}, fmt.Errorf("chart %q: %w", chartName, err)
				}
				version.URLs = append(version.URLs, advertisedPath)
				if _, exists := references[advertisedPath]; !exists {
					references[advertisedPath] = repository.PackageReference{Source: source.name, Key: key}
				}
			}
			aggregate.Add(version)
		}
	}

	result := repository.Contribution{}
	for _, versions := range aggregate.Finalize() {
		for _, version := range versions {
			discovered := repository.DiscoveredChartVersion{Version: version}
			for _, advertisedPath := range version.URLs {
				discovered.Packages = append(discovered.Packages, repository.DiscoveredPackage{
					AdvertisedPath: advertisedPath,
					Reference:      references[advertisedPath],
				})
			}
			result.ChartVersions = append(result.ChartVersions, discovered)
			result.IndexedCount++
		}
	}
	return result, nil
}

func (source *Source) OpenPackage(ctx context.Context, reference repository.PackageReference) (repository.ChartPackage, error) {
	parts, ok := pathParts(reference.Key)
	if reference.Source != source.name || !ok {
		return repository.ChartPackage{}, repository.ErrPackageNotFound
	}

	filename := parts[len(parts)-1]
	var body io.ReadCloser
	var err error
	if parts[0] == "package-in-branch" {
		body, err = source.backend.FetchBranchFile(ctx, source.owner, source.repo, source.branch, strings.Join(parts[1:], "/"), source.token)
	} else {
		body, err = source.backend.DownloadRelease(ctx, source.owner, source.repo, strings.Join(parts[:len(parts)-1], "/"), filename, source.token)
	}
	if err != nil {
		return repository.ChartPackage{}, err
	}
	return repository.ChartPackage{Body: body, Filename: filename}, nil
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

func rewriteURL(rawURL, owner, repo, sourceName string) (advertisedPath, key string, err error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || rawURL == "" || parsed.Query().Encode() != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("unsupported chart URL %q", rawURL)
	}

	if parsed.Scheme != "" || parsed.Host != "" {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || len(parts) < 6 || parts[0] != owner || parts[1] != repo || parts[2] != "releases" || parts[3] != "download" || !validPackageParts(parts[4:]) {
			return "", "", fmt.Errorf("unsupported chart URL %q", rawURL)
		}
		key = escapePath(parts[4:])
		return "charts/" + sourceName + "/" + key, key, nil
	}

	if parsed.Path == "" || strings.HasPrefix(parsed.Path, "/") {
		return "", "", fmt.Errorf("unsupported chart URL %q", rawURL)
	}
	parts := strings.Split(parsed.Path, "/")
	if !validPackageParts(parts) {
		return "", "", fmt.Errorf("unsupported chart URL %q", rawURL)
	}
	key = "package-in-branch/" + escapePath(parts)
	return "charts/" + sourceName + "/" + key, key, nil
}

func validPackageParts(parts []string) bool {
	if len(parts) == 0 || !strings.HasSuffix(parts[len(parts)-1], ".tgz") {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\\`) {
			return false
		}
	}
	return true
}

func escapePath(parts []string) string {
	encoded := make([]string, len(parts))
	for index, part := range parts {
		encoded[index] = url.PathEscape(part)
	}
	return strings.Join(encoded, "/")
}

func pathParts(rawPath string) ([]string, bool) {
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
	return parts, validPackageParts(parts)
}

var _ repository.Discovery = (*Source)(nil)
var _ repository.PackageOpener = (*Source)(nil)
