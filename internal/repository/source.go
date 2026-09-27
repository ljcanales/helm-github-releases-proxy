// Package repository owns aggregation, publication, refresh, and package lookup
// for the served Helm repository.
package repository

import (
	"context"
	"io"

	"helm-github-releases-proxy/internal/chart"
)

type SourceKind string

const (
	GitHubReleasesSource SourceKind = "github-releases"
	ChartReleaserSource  SourceKind = "chart-releaser"
	LocalSource          SourceKind = "local-directory"
)

// SourceConfig is the safe source description needed for coordination and status.
// Credentials and source-specific retrieval settings remain in source implementations.
type SourceConfig struct {
	Name   string
	Kind   SourceKind
	Owner  string
	Repo   string
	Branch string
	Path   string
}

type Discovery interface {
	Discover(context.Context) (Contribution, error)
}

type PackageOpener interface {
	OpenPackage(context.Context, PackageReference) (ChartPackage, error)
}

// PackageReference is opaque to repository coordination. Key is interpreted only
// by the source that created it.
type PackageReference struct {
	Source string
	Key    string
}

type DiscoveredPackage struct {
	AdvertisedPath string
	Reference      PackageReference
}

type DiscoveredChartVersion struct {
	Version  chart.Version
	Packages []DiscoveredPackage
}

type Contribution struct {
	ChartVersions []DiscoveredChartVersion
	IndexedCount  int
	SkippedCount  int
}

type ChartPackage struct {
	Body     io.ReadCloser
	Filename string
}

type Source struct {
	Config    SourceConfig
	Discovery Discovery
	Packages  PackageOpener
}

type Sources []Source
