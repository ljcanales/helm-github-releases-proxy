package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestLoadGitHubReleasesFlatConfiguration(t *testing.T) {
	for _, mode := range []string{"", "github-releases"} {
		t.Run("mode="+mode, func(t *testing.T) {
			cfg, err := LoadWithLookup(mapLookup(map[string]string{
				"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts",
				"CHART_RELEASER_PAGES_BRANCH": "ignored",
			}))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Port != 8080 || cfg.CacheTTLSeconds != 60 || cfg.LogLevel != slog.LevelInfo {
				t.Fatalf("defaults = %#v", cfg)
			}
			if len(cfg.Repositories) != 1 {
				t.Fatalf("sources = %#v", cfg.Repositories)
			}
			source := cfg.Repositories[0]
			if source.Name != "github-releases" || source.Type != GitHubReleasesType || source.Owner != "acme" || source.Repo != "charts" || source.Branch != "" {
				t.Fatalf("source = %#v", source)
			}
		})
	}
}

func TestLoadChartReleaserFlatConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		branch string
		token  string
	}{
		{
			name: "default branch without token",
			values: map[string]string{
				"MODE": "chart-releaser", "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts",
			},
			branch: "gh-pages",
		},
		{
			name: "default branch with empty token",
			values: map[string]string{
				"MODE": "chart-releaser", "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "GITHUB_TOKEN": "",
			},
			branch: "gh-pages",
		},
		{
			name: "custom branch with token",
			values: map[string]string{
				"MODE": " chart-releaser ", "GITHUB_OWNER": " acme ", "GITHUB_REPO": " charts ",
				"CHART_RELEASER_PAGES_BRANCH": " release-pages ", "GITHUB_TOKEN": " secret ",
			},
			branch: "release-pages",
			token:  " secret ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadWithLookup(mapLookup(tc.values))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Repositories) != 1 {
				t.Fatalf("sources = %#v", cfg.Repositories)
			}
			source := cfg.Repositories[0]
			if source.Name != "chart-releaser" || source.Type != ChartReleaserType || source.Owner != "acme" || source.Repo != "charts" || source.Branch != tc.branch || source.GitHubToken != tc.token {
				t.Fatalf("source = %#v", source)
			}
		})
	}
}

func TestLoadGitHubModesWithOptionalLocalAggregation(t *testing.T) {
	for _, mode := range []string{"github-releases", "chart-releaser"} {
		t.Run(mode+" with local charts", func(t *testing.T) {
			path := t.TempDir()
			cfg, err := LoadWithLookup(mapLookup(map[string]string{
				"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "LOCAL_PATH": " " + path + " ",
			}))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Repositories) != 2 {
				t.Fatalf("sources = %#v", cfg.Repositories)
			}
			if cfg.Repositories[0].Name != mode || cfg.Repositories[0].Type != RepositoryType(mode) {
				t.Fatalf("GitHub source = %#v", cfg.Repositories[0])
			}
			if local := cfg.Repositories[1]; local.Name != "local" || local.Type != LocalDirectoryType || local.Path != path {
				t.Fatalf("local source = %#v", local)
			}
		})

		t.Run(mode+" without local charts", func(t *testing.T) {
			for _, path := range []string{"", " "} {
				cfg, err := LoadWithLookup(mapLookup(map[string]string{
					"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "LOCAL_PATH": path,
				}))
				if err != nil {
					t.Fatal(err)
				}
				if len(cfg.Repositories) != 1 || cfg.Repositories[0].Name != mode {
					t.Fatalf("sources = %#v", cfg.Repositories)
				}
			}
		})

		t.Run(mode+" rejects relative local path", func(t *testing.T) {
			_, err := LoadWithLookup(mapLookup(map[string]string{
				"MODE": mode, "GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "LOCAL_PATH": "relative/charts",
			}))
			if err == nil || !strings.Contains(err.Error(), "LOCAL_PATH") {
				t.Fatalf("error = %v, want LOCAL_PATH validation error", err)
			}
		})
	}
}

func TestLoadLocalOnlyConfiguration(t *testing.T) {
	path := t.TempDir()
	values := map[string]string{
		"MODE":                        " local-only ",
		"LOCAL_PATH":                  " " + path + " ",
		"GITHUB_OWNER":                "ignored-owner",
		"GITHUB_REPO":                 "ignored-repository",
		"GITHUB_TOKEN":                "ignored-token",
		"CHART_RELEASER_PAGES_BRANCH": "ignored-branch",
	}
	cfg, err := LoadWithLookup(func(name string) (string, bool) {
		switch name {
		case "GITHUB_OWNER", "GITHUB_REPO", "GITHUB_TOKEN", "CHART_RELEASER_PAGES_BRANCH":
			t.Errorf("local-only configuration looked up inactive setting %s", name)
		}
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 {
		t.Fatalf("sources = %#v", cfg.Repositories)
	}
	source := cfg.Repositories[0]
	if source.Name != "local" || source.Type != LocalDirectoryType || source.Path != path || source.Owner != "" || source.Repo != "" || source.Branch != "" || source.GitHubToken != "" {
		t.Fatalf("source = %#v", source)
	}
}

func TestLoadLocalOnlyRequiresAbsoluteLocalPath(t *testing.T) {
	for _, path := range []string{"", " ", "relative/charts"} {
		t.Run("path="+path, func(t *testing.T) {
			_, err := LoadWithLookup(mapLookup(map[string]string{
				"MODE":         "local-only",
				"LOCAL_PATH":   path,
				"GITHUB_OWNER": "ignored-owner",
				"GITHUB_REPO":  "ignored-repository",
			}))
			if err == nil || !strings.Contains(err.Error(), "LOCAL_PATH") {
				t.Fatalf("error = %v, want LOCAL_PATH validation error", err)
			}
		})
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func TestLoadRejectsInvalidFlatSettings(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"GITHUB_OWNER", ""}, {"GITHUB_REPO", " "},
		{"MODE", "local-directory"}, {"MODE", "unknown"},
		{"PORT", "0"}, {"PORT", "65536"}, {"PORT", "abc"},
		{"CACHE_TTL_SECONDS", "-1"}, {"CACHE_TTL_SECONDS", "1.5"}, {"LOG_LEVEL", "invalid"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			values := map[string]string{"GITHUB_OWNER": "acme", "GITHUB_REPO": "charts"}
			values[tc.key] = tc.value
			cfg, err := LoadWithLookup(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %v, want %s validation error", err, tc.key)
			}
			if cfg.Port != 8080 || cfg.CacheTTLSeconds != 60 || cfg.LogLevel != slog.LevelInfo {
				t.Fatalf("fallback defaults = %#v", cfg)
			}
		})
	}
}

func TestLoadPreservesOperationalSettings(t *testing.T) {
	for _, level := range []struct {
		value string
		want  slog.Level
	}{{"debug", slog.LevelDebug}, {"INFO", slog.LevelInfo}, {"WARN", slog.LevelWarn}, {"warning", slog.LevelWarn}, {"ERROR", slog.LevelError}} {
		cfg, err := LoadWithLookup(mapLookup(map[string]string{"GITHUB_OWNER": "acme", "GITHUB_REPO": "charts", "PORT": " 9090 ", "CACHE_TTL_SECONDS": "0", "LOG_LEVEL": level.value}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 9090 || cfg.CacheTTLSeconds != 0 || cfg.LogLevel != level.want {
			t.Fatalf("settings = %#v", cfg)
		}
	}
}
