package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultPort = 8080

// Config contains the global settings and configured chart sources.
type Config struct {
	Port            int
	CacheTTLSeconds int
	LogLevel        slog.Level
	Sources         Sources
}

type SourceKind string

const (
	GitHubReleasesKind SourceKind = "github-releases"
	ChartReleaserKind  SourceKind = "chart-releaser"
	LocalSourceKind    SourceKind = "local-directory"
)

// SourceConfig is the validated, kind-specific configuration for one chart
// source. Fields not used by a source kind remain empty.
type SourceConfig struct {
	Name        string
	Kind        SourceKind
	Owner       string
	Repo        string
	Branch      string
	Path        string
	GitHubToken string
}

type Sources []SourceConfig

// Load reads the settings currently supported by the Go service. It returns a
// usable config along with an error so the HTTP server can expose /readyz even
// when an operator has supplied invalid configuration.
func Load(getenv func(string) string) (Config, error) {
	return LoadWithLookup(func(name string) (string, bool) {
		value := getenv(name)
		return value, value != ""
	})
}

// LoadWithLookup reads flat environment settings for the selected chart source.
func LoadWithLookup(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{Port: defaultPort, CacheTTLSeconds: 60, LogLevel: slog.LevelInfo}
	cfg.Sources = Sources{}
	var firstErr error

	if value, ok := lookup("PORT"); ok && strings.TrimSpace(value) != "" {
		value = strings.TrimSpace(value)
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			firstErr = fmt.Errorf("PORT must be an integer between 1 and 65535")
		} else {
			cfg.Port = port
		}
	}

	if value, ok := lookup("CACHE_TTL_SECONDS"); ok && strings.TrimSpace(value) != "" {
		value = strings.TrimSpace(value)
		ttl, err := strconv.Atoi(value)
		if err != nil || ttl < 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("CACHE_TTL_SECONDS must be a non-negative integer")
			}
		} else {
			cfg.CacheTTLSeconds = ttl
		}
	}

	if value, ok := lookup("LOG_LEVEL"); ok && strings.TrimSpace(value) != "" {
		value = strings.TrimSpace(value)
		level, err := parseLevel(value)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			cfg.LogLevel = level
		}
	}

	mode, _ := lookup("MODE")
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = string(GitHubReleasesKind)
	}
	var sourceErr error
	switch mode {
	case string(GitHubReleasesKind):
		owner, repo, err := githubOwnerAndRepo(lookup, mode)
		if err != nil {
			sourceErr = err
		} else {
			token, _ := lookup("GITHUB_TOKEN")
			cfg.Sources = Sources{{Name: mode, Kind: GitHubReleasesKind, Owner: owner, Repo: repo, GitHubToken: token}}
			sourceErr = appendOptionalLocalSource(&cfg, lookup)
		}
	case string(ChartReleaserKind):
		owner, repo, err := githubOwnerAndRepo(lookup, mode)
		if err != nil {
			sourceErr = err
		} else {
			branch, _ := lookup("CHART_RELEASER_PAGES_BRANCH")
			branch = strings.TrimSpace(branch)
			if branch == "" {
				branch = "gh-pages"
			}
			token, _ := lookup("GITHUB_TOKEN")
			cfg.Sources = Sources{{Name: mode, Kind: ChartReleaserKind, Owner: owner, Repo: repo, Branch: branch, GitHubToken: token}}
			sourceErr = appendOptionalLocalSource(&cfg, lookup)
		}
	case "local-only":
		path, _ := lookup("LOCAL_PATH")
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) {
			sourceErr = fmt.Errorf("local-only requires a nonempty absolute LOCAL_PATH")
		} else {
			cfg.Sources = Sources{{Name: "local", Kind: LocalSourceKind, Path: path}}
		}
	default:
		sourceErr = fmt.Errorf("unsupported MODE %q", mode)
	}
	if firstErr == nil {
		firstErr = sourceErr
	}
	return cfg, firstErr
}

func appendOptionalLocalSource(cfg *Config, lookup func(string) (string, bool)) error {
	path, _ := lookup("LOCAL_PATH")
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("LOCAL_PATH must be absolute when supplied")
	}
	cfg.Sources = append(cfg.Sources, SourceConfig{Name: "local", Kind: LocalSourceKind, Path: path})
	return nil
}

func githubOwnerAndRepo(lookup func(string) (string, bool), mode string) (string, string, error) {
	owner, _ := lookup("GITHUB_OWNER")
	repo, _ := lookup("GITHUB_REPO")
	owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("%s requires GITHUB_OWNER and GITHUB_REPO", mode)
	}
	return owner, repo, nil
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToUpper(value) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("LOG_LEVEL must be one of DEBUG, INFO, WARN, or ERROR")
	}
}

// Environment is the production environment lookup used by main.
func Environment(name string) string { return os.Getenv(name) }

// EnvironmentLookup is the production lookup that preserves variable presence.
func EnvironmentLookup(name string) (string, bool) { return os.LookupEnv(name) }
