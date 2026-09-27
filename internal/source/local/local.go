// Package local supplies charts stored in one local filesystem directory.
package local

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"helm-github-releases-proxy/internal/chart"
	"helm-github-releases-proxy/internal/repository"
)

// Source discovers and opens chart archives owned by one configured source.
type Source struct {
	name      string
	directory string
	logger    *slog.Logger
}

func New(name, directory string, logger *slog.Logger) *Source {
	return &Source{name: name, directory: directory, logger: logger}
}

func (source *Source) Discover(_ context.Context) (repository.Contribution, error) {
	archives, skipped, err := scan(source.directory, source.logger)
	if err != nil {
		return repository.Contribution{}, localPathError{configuredPath: source.directory, err: err}
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].filename < archives[j].filename })
	result := repository.Contribution{IndexedCount: len(archives), SkippedCount: skipped}
	for _, archive := range archives {
		advertisedPath := "charts/" + source.name + "/" + url.PathEscape(archive.filename)
		result.ChartVersions = append(result.ChartVersions, repository.DiscoveredChartVersion{
			Version: chart.Version{
				APIVersion: "v2",
				Name:       archive.name,
				Version:    archive.version,
				Created:    archive.created.UTC().Format(time.RFC3339Nano),
				Digest:     archive.digest,
			},
			Packages: []repository.DiscoveredPackage{{
				AdvertisedPath: advertisedPath,
				Reference:      repository.PackageReference{Source: source.name, Key: archive.filename},
			}},
		})
	}
	return result, nil
}

// OpenPackage validates source ownership and current filesystem eligibility.
// Published membership is checked by repository before this method is called.
func (source *Source) OpenPackage(_ context.Context, reference repository.PackageReference) (repository.ChartPackage, error) {
	filename := reference.Key
	if reference.Source != source.name || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) || filename == "." || filename == ".." {
		return repository.ChartPackage{}, repository.ErrPackageNotFound
	}
	root, err := filepath.Abs(source.directory)
	if err != nil {
		return repository.ChartPackage{}, err
	}
	path := filepath.Join(root, filename)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return repository.ChartPackage{}, repository.ErrPackageNotFound
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return repository.ChartPackage{}, repository.ErrPackageNotFound
		}
		return repository.ChartPackage{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return repository.ChartPackage{}, repository.ErrPackageNotFound
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return repository.ChartPackage{}, repository.ErrPackageNotFound
		}
		return repository.ChartPackage{}, err
	}
	return repository.ChartPackage{Body: file, Filename: filename}, nil
}

var _ repository.Discovery = (*Source)(nil)
var _ repository.PackageOpener = (*Source)(nil)

type localPathError struct {
	configuredPath string
	err            error
}

func (err localPathError) Error() string {
	if err.configuredPath == "" {
		return err.err.Error()
	}
	return strings.ReplaceAll(err.err.Error(), err.configuredPath, "<local-path>")
}

func (err localPathError) Unwrap() error { return err.err }
