// Package app assembles configuration, sources, the served repository, and HTTP transport.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"helm-github-releases-proxy/internal/config"
	"helm-github-releases-proxy/internal/httpapi"
	"helm-github-releases-proxy/internal/logging"
	"helm-github-releases-proxy/internal/repository"
	"helm-github-releases-proxy/internal/source/chartreleaser"
	"helm-github-releases-proxy/internal/source/githubreleases"
	"helm-github-releases-proxy/internal/source/local"
)

type GitHubBackend interface {
	githubreleases.Backend
	chartreleaser.Backend
}

type Option func(*assembly)

// WithSource replaces or appends a source during assembly. It is primarily useful
// for injecting a deterministic source at the assembled HTTP boundary.
func WithSource(source repository.Source) Option {
	return func(value *assembly) {
		for index := range value.sources {
			if value.sources[index].Config.Name == source.Config.Name {
				value.sources[index] = source
				return
			}
		}
		value.sources = append(value.sources, source)
	}
}

type assembly struct{ sources repository.Sources }

type Runtime struct {
	Handler     http.Handler
	repository  *repository.Service
	configValid bool
	events      logging.Logger
}

func New(cfg config.Config, configErr error, logger *slog.Logger, github GitHubBackend, now func() time.Time, refreshContext context.Context, options ...Option) *Runtime {
	assembled := assembly{sources: configuredSources(cfg, logger, github)}
	for _, option := range options {
		option(&assembled)
	}
	events := logging.New(logger)
	servedRepository := repository.New(assembled.sources, repository.WithTTL(time.Duration(cfg.CacheTTLSeconds)*time.Second), repository.WithClock(now), repository.WithRefreshContext(refreshContext), repository.WithLogger(events))
	return &Runtime{Handler: httpapi.LogRequests(httpapi.NewPublished(servedRepository, configErr == nil).Handler(), events), repository: servedRepository, configValid: configErr == nil, events: events}
}

func (runtime *Runtime) Start() {
	if runtime.configValid {
		runtime.repository.Start(func(err error) {
			runtime.events.Warn(context.Background(), "index startup warm failed", "error", err)
		})
	}
}

func configuredSources(cfg config.Config, logger *slog.Logger, github GitHubBackend) repository.Sources {
	configured := make(repository.Sources, 0, len(cfg.Sources))
	for _, sourceConfig := range cfg.Sources {
		descriptor := repository.SourceConfig{Name: sourceConfig.Name, Kind: repository.SourceKind(sourceConfig.Kind), Owner: sourceConfig.Owner, Repo: sourceConfig.Repo, Branch: sourceConfig.Branch}
		if sourceConfig.Kind == config.LocalSourceKind {
			descriptor.Path = filepath.Base(filepath.Clean(sourceConfig.Path))
			source := local.New(sourceConfig.Name, sourceConfig.Path, logger)
			configured = append(configured, repository.Source{Config: descriptor, Discovery: source, Packages: source})
			continue
		}
		if sourceConfig.Kind == config.GitHubReleasesKind {
			source := githubreleases.New(sourceConfig.Name, sourceConfig.Owner, sourceConfig.Repo, github, logger, githubreleases.WithToken(sourceConfig.GitHubToken))
			configured = append(configured, repository.Source{Config: descriptor, Discovery: source, Packages: source})
			continue
		}
		switch sourceConfig.Kind {
		case config.ChartReleaserKind:
			source := chartreleaser.New(sourceConfig.Name, sourceConfig.Owner, sourceConfig.Repo, sourceConfig.Branch, github, chartreleaser.WithToken(sourceConfig.GitHubToken))
			configured = append(configured, repository.Source{Config: descriptor, Discovery: source, Packages: source})
		default:
			continue
		}
	}
	return configured
}
