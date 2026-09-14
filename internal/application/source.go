// Package application defines the contracts consumed by aggregate coordination.
package application

import (
	"context"
	"io"

	"helm-github-releases-proxy/internal/catalog"
)

// ChartDiscovery discovers one configured source. Configuration belongs to the
// implementation; refresh lifetime and published cache state belong to the caller.
// An error rejects the entire contribution, even if a partial result is returned.
type ChartDiscovery interface {
	Discover(context.Context) (SourceContribution, error)
}

// PackageOpener opens a source-owned package reference. Implementations retain
// source-specific eligibility and validation (including direct chart-releaser
// paths). The caller closes the returned body.
type PackageOpener interface {
	OpenPackage(context.Context, PackageReference) (ChartPackage, error)
}

// PackageReference identifies bytes within a source, rather than a chart version.
// Key is opaque to the application and need not be a URL, path, or asset ID.
// Different references may supply different bytes for the same chart identity.
type PackageReference struct {
	Source string
	Key    string
}

// DiscoveredChartVersion pairs metadata with the packages that supply it.
// Packages preserve source URL order; their references support package lookup.
type DiscoveredChartVersion struct {
	Version  catalog.ChartVersion
	Packages []DiscoveredPackage
}

type DiscoveredPackage struct {
	URL       string
	Reference PackageReference
}

// SourceContribution contains only one discovery attempt, never source cache
// state. IndexedCount retains source-specific counting semantics (assets, local
// archives, or distinct chart-releaser versions), rather than merged counts.
type SourceContribution struct {
	ChartVersions []DiscoveredChartVersion
	IndexedCount  int
	SkippedCount  int
}

// ChartPackage contains an opened archive and its download filename.
type ChartPackage struct {
	Body     io.ReadCloser
	Filename string
}
