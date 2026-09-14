// Package startup assembles the configured sources, coordinator, and HTTP boundary.
package startup

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"helm-github-releases-proxy/internal/application"
	"helm-github-releases-proxy/internal/config"
	"helm-github-releases-proxy/internal/httpapi"
	"helm-github-releases-proxy/internal/sources"
)

// GitHubBackend combines the transport contracts needed by the supported GitHub modes.
type GitHubBackend interface {
	sources.GitHubReleaseBackend
	sources.ChartReleaserBackend
}

// Runtime contains the assembled HTTP handler and startup warming lifecycle.
type Runtime struct {
	Handler     http.Handler
	coordinator *application.Service
	configValid bool
	logger      *slog.Logger
}

// New assembles dependencies without acquiring charts. The clock and refresh
// context are explicit; downloads use their requesting client's context.
func New(cfg config.Config, configErr error, logger *slog.Logger, github GitHubBackend, now func() time.Time, refreshContext context.Context) *Runtime {
	configured := make([]application.Source, 0, len(cfg.Repositories))
	for _, repository := range cfg.Repositories {
		var source interface {
			application.ChartDiscovery
			application.PackageOpener
		}
		switch repository.Type {
		case config.LocalDirectoryType:
			source = sources.NewLocal(repository.Name, repository.Path, logger)
		case config.GitHubReleasesType:
			source = sources.NewGitHubReleases(repository, github, logger)
		case config.ChartReleaserType:
			source = sources.NewChartReleaser(repository, github)
		default:
			continue
		}
		configured = append(configured, application.Source{Repository: repository, Discovery: source, Packages: source})
	}
	coordinator := application.New(configured, time.Duration(cfg.CacheTTLSeconds)*time.Second, now, refreshContext)
	return &Runtime{
		Handler:     httpapi.New(coordinator, configErr == nil).Handler(),
		coordinator: coordinator,
		configValid: configErr == nil,
		logger:      logger,
	}
}

// Start preserves configuration-only readiness and best-effort asynchronous warming.
func (r *Runtime) Start() {
	if r.configValid {
		r.coordinator.Start(func(err error) { r.logger.Warn("index startup warm failed", "error", err) })
	}
}
