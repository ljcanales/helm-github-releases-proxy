// Package sources implements chart discovery and package access.
package sources

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"helm-github-releases-proxy/internal/application"
	"helm-github-releases-proxy/internal/catalog"
	"helm-github-releases-proxy/internal/localcharts"
)

// Local reads a configured directory. It holds no published package lookup or
// refresh state; the caller owns membership and aggregate publication.
type Local struct {
	name      string
	directory string
	logger    *slog.Logger
}

func NewLocal(name, directory string, logger *slog.Logger) *Local {
	return &Local{name: name, directory: directory, logger: logger}
}

func (s *Local) Discover(_ context.Context) (application.SourceContribution, error) {
	charts, skipped, err := localcharts.Scan(s.directory, s.logger)
	if err != nil {
		return application.SourceContribution{}, err
	}
	sort.Slice(charts, func(i, j int) bool { return charts[i].Filename < charts[j].Filename })
	result := application.SourceContribution{IndexedCount: len(charts), SkippedCount: skipped}
	for _, chart := range charts {
		chartURL := "charts/" + s.name + "/" + chart.Filename
		result.ChartVersions = append(result.ChartVersions, application.DiscoveredChartVersion{
			Version:  catalog.ChartVersion{APIVersion: "v2", Name: chart.Name, Version: chart.Version, Created: chart.Created.UTC().Format(time.RFC3339Nano), Digest: chart.Digest, URLs: []string{chartURL}},
			Packages: []application.DiscoveredPackage{{URL: chartURL, Reference: application.PackageReference{Source: s.name, Key: chart.Filename}}},
		})
	}
	return result, nil
}

// OpenPackage validates source ownership and filesystem eligibility. Published
// lookup membership must be checked by the caller before opening a reference.
func (s *Local) OpenPackage(_ context.Context, reference application.PackageReference) (application.ChartPackage, error) {
	filename := reference.Key
	if reference.Source != s.name || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\\`) || filename == "." || filename == ".." {
		return application.ChartPackage{}, os.ErrNotExist
	}
	root, err := filepath.Abs(s.directory)
	if err != nil {
		return application.ChartPackage{}, err
	}
	path := filepath.Join(root, filename)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return application.ChartPackage{}, os.ErrNotExist
	}
	info, err := os.Lstat(path)
	if err != nil {
		return application.ChartPackage{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return application.ChartPackage{}, os.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return application.ChartPackage{}, err
	}
	return application.ChartPackage{Body: file, Filename: filename}, nil
}
