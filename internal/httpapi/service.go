package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"helm-github-releases-proxy/internal/config"
	githubclient "helm-github-releases-proxy/internal/github"
	"helm-github-releases-proxy/internal/localcharts"

	"gopkg.in/yaml.v3"
)

// Service is the HTTP boundary for the initial Go rewrite.
type Service struct {
	configValid  bool
	config       config.Config
	logger       *slog.Logger
	now          func() time.Time
	mu           sync.RWMutex
	buildMu      sync.Mutex
	localFiles   map[string]map[string]localcharts.Chart
	lastIndex    []byte
	cachedAt     time.Time
	expiresAt    time.Time
	lastAttempt  time.Time
	lastSuccess  time.Time
	lastError    string
	stale        bool
	repositories []repositoryStatus
	warmOnce     sync.Once
	github       githubBackend
	githubFiles  map[string]map[int64]string
}

type githubBackend interface {
	ListReleases(context.Context, string, string, string) ([]githubclient.Release, error)
	DownloadAsset(context.Context, string, string, int64, string) (io.ReadCloser, error)
}

type chartReleaserBackend interface {
	FetchBranchFile(context.Context, string, string, string, string, string) (io.ReadCloser, error)
	DownloadRelease(context.Context, string, string, string, string, string) (io.ReadCloser, error)
}

func New(cfg config.Config, configErr error, logger *slog.Logger) *Service {
	repositories := make([]repositoryStatus, 0, len(cfg.Repositories))
	for _, repository := range cfg.Repositories {
		repositories = append(repositories, newRepositoryStatus(repository))
	}
	return &Service{
		configValid:  configErr == nil,
		config:       cfg,
		logger:       logger,
		now:          time.Now,
		localFiles:   make(map[string]map[string]localcharts.Chart),
		github:       githubclient.NewClient(nil),
		githubFiles:  make(map[string]map[int64]string),
		repositories: repositories,
	}
}

// Start begins the best-effort asynchronous startup warm. Readiness remains
// based solely on configuration validity and does not wait for this work.
func (s *Service) Start() {
	s.warmOnce.Do(func() {
		if s.configValid {
			go func() {
				if _, err := s.refresh(); err != nil {
					s.logger.Warn("index startup warm failed", "error", err)
				}
			}()
		}
	})
}

// Handler returns the service router. chi is deliberately kept at the edge so
// later repository routes can be added without changing the server entrypoint.
func (s *Service) Handler() http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", s.health)
	router.Get("/readyz", s.ready)
	router.Get("/index.yaml", s.index)
	router.Get("/status", s.status)
	router.Get("/charts/{repository}/{asset}/{filename}", s.chart)
	router.Get("/charts/{repository}/{filename}", s.chart)
	router.Get("/charts/{repository}/*", s.chartReleaser)
	return router
}

func (s *Service) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, `{"status":"ok"}`)
}

func (s *Service) ready(writer http.ResponseWriter, _ *http.Request) {
	if !s.configValid {
		writeJSON(writer, http.StatusServiceUnavailable, `{"status":"not_ready"}`)
		return
	}
	writeJSON(writer, http.StatusOK, `{"status":"ok"}`)
}

func (s *Service) index(writer http.ResponseWriter, _ *http.Request) {
	now := s.now().UTC()
	s.mu.RLock()
	if len(s.lastIndex) > 0 && !s.expiresAt.IsZero() && now.Before(s.expiresAt) {
		body := append([]byte(nil), s.lastIndex...)
		s.mu.RUnlock()
		s.writeIndex(writer, body)
		return
	}
	s.mu.RUnlock()

	body, err := s.refresh()
	if err != nil {
		writePlainError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeIndex(writer, body)
}

func (s *Service) writeIndex(writer http.ResponseWriter, body []byte) {
	writer.Header().Set("Content-Type", "application/x-yaml")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func (s *Service) refresh() ([]byte, error) {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	now := s.now().UTC()
	s.mu.RLock()
	if len(s.lastIndex) > 0 && !s.expiresAt.IsZero() && now.Before(s.expiresAt) {
		body := append([]byte(nil), s.lastIndex...)
		s.mu.RUnlock()
		return body, nil
	}
	s.mu.RUnlock()

	entries := make(map[string]map[string]*localIndexEntry)
	indexed := make(map[string]map[string]localcharts.Chart)
	githubIndexed := make(map[string]map[int64]string)
	failedRepositories := make([]string, 0)
	repositoryStatuses := make([]repositoryStatus, 0, len(s.config.Repositories))
	for _, repository := range s.config.Repositories {
		repositoryStatus := newRepositoryStatus(repository)
		if repository.Type == config.GitHubReleasesType {
			releases, err := s.github.ListReleases(context.Background(), repository.Owner, repository.Repo, repository.GitHubToken)
			if err != nil {
				failedRepositories = append(failedRepositories, repository.Name)
				repositoryStatus.Status = "failed"
				repositoryStatus.Error = sanitizeBackendError(err, repository.Path)
				repositoryStatuses = append(repositoryStatuses, repositoryStatus)
				s.logger.Warn("GitHub releases unavailable", "repository", repository.Name, "error", err)
				continue
			}
			githubIndexed[repository.Name] = make(map[int64]string)
			skippedAssets := 0
			for _, release := range releases {
				for _, asset := range release.Assets {
					if !strings.HasSuffix(asset.Name, ".tgz") {
						skippedAssets++
						s.logger.Debug("skipping non-chart GitHub release asset", "repository", repository.Name, "filename", asset.Name)
						continue
					}
					name, version, ok := parseGitHubChartFilename(asset.Name)
					if !ok {
						skippedAssets++
						s.logger.Debug("skipping invalid GitHub chart filename", "repository", repository.Name, "filename", asset.Name)
						continue
					}
					githubIndexed[repository.Name][asset.ID] = asset.Name
					addGitHubEntry(entries, repository.Name, name, version, asset, release.PublishedAt)
					repositoryStatus.IndexedCount++
				}
			}
			if skippedAssets > 0 {
				s.logger.Debug("skipped GitHub release assets", "repository", repository.Name, "count", skippedAssets)
			}
			repositoryStatus.SkippedCount = skippedAssets
			repositoryStatuses = append(repositoryStatuses, repositoryStatus)
			continue
		}
		if repository.Type == config.ChartReleaserType {
			backend, ok := s.github.(chartReleaserBackend)
			if !ok {
				failedRepositories = append(failedRepositories, repository.Name)
				repositoryStatus.Status = "failed"
				repositoryStatus.Error = "backend is unavailable"
				repositoryStatuses = append(repositoryStatuses, repositoryStatus)
				s.logger.Warn("chart-releaser backend unavailable", "repository", repository.Name)
				continue
			}
			indexFile, err := backend.FetchBranchFile(context.Background(), repository.Owner, repository.Repo, repository.Branch, "index.yaml", repository.GitHubToken)
			if err != nil {
				failedRepositories = append(failedRepositories, repository.Name)
				repositoryStatus.Status = "failed"
				repositoryStatus.Error = sanitizeBackendError(err, repository.Path)
				repositoryStatuses = append(repositoryStatuses, repositoryStatus)
				s.logger.Warn("chart-releaser index unavailable", "repository", repository.Name, "error", err)
				continue
			}
			data, readErr := io.ReadAll(indexFile)
			_ = indexFile.Close()
			if readErr != nil {
				failedRepositories = append(failedRepositories, repository.Name)
				repositoryStatus.Status = "failed"
				repositoryStatus.Error = sanitizeBackendError(readErr, repository.Path)
				repositoryStatuses = append(repositoryStatuses, repositoryStatus)
				s.logger.Warn("chart-releaser index read failed", "repository", repository.Name, "error", readErr)
				continue
			}
			backendEntries := make(map[string]map[string]*localIndexEntry)
			if err := addChartReleaserEntries(backendEntries, repository.Name, repository.Owner, repository.Repo, data); err != nil {
				failedRepositories = append(failedRepositories, repository.Name)
				repositoryStatus.Status = "failed"
				repositoryStatus.Error = sanitizeBackendError(err, repository.Path)
				repositoryStatuses = append(repositoryStatuses, repositoryStatus)
				s.logger.Warn("chart-releaser index invalid", "repository", repository.Name, "error", err)
			} else {
				mergeIndexEntries(entries, backendEntries)
				repositoryStatus.IndexedCount = countRepositoryEntries(backendEntries, repository.Name)
				repositoryStatuses = append(repositoryStatuses, repositoryStatus)
			}
			continue
		}
		if repository.Type != config.LocalDirectoryType {
			continue
		}
		charts, skipped, err := localcharts.Scan(repository.Path, s.logger)
		if err != nil {
			failedRepositories = append(failedRepositories, repository.Name)
			repositoryStatus.Status = "failed"
			repositoryStatus.Error = sanitizeBackendError(err, repository.Path)
			repositoryStatuses = append(repositoryStatuses, repositoryStatus)
			s.logger.Warn("local chart directory unavailable", "repository", repository.Name, "path", repository.Path, "error", err)
			continue
		}
		indexed[repository.Name] = make(map[string]localcharts.Chart, len(charts))
		sort.Slice(charts, func(i, j int) bool { return charts[i].Filename < charts[j].Filename })
		for _, chart := range charts {
			indexed[repository.Name][chart.Filename] = chart
			addLocalEntry(entries, repository.Name, chart)
		}
		repositoryStatus.IndexedCount = len(charts)
		repositoryStatus.SkippedCount = skipped
		repositoryStatuses = append(repositoryStatuses, repositoryStatus)
	}
	bodyData := localIndex{APIVersion: "v1", Generated: now.Format(time.RFC3339Nano), Entries: finalizeEntries(entries)}
	body, err := yaml.Marshal(bodyData)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = now
	s.lastError = ""
	s.repositories = repositoryStatuses
	if len(failedRepositories) > 0 {
		s.lastError = "repositories failed: " + strings.Join(failedRepositories, ", ")
		if len(s.lastIndex) > 0 {
			body = append([]byte(nil), s.lastIndex...)
			s.stale = true
		} else {
			// A partial build is still the best available aggregate when no
			// previous index exists. Keep it for later stale fallback, but do
			// not give the failed attempt a fresh TTL.
			s.lastIndex = append([]byte(nil), body...)
			s.localFiles = indexed
			s.githubFiles = githubIndexed
			s.stale = false
		}
		return body, nil
	}

	s.lastIndex = append([]byte(nil), body...)
	s.localFiles = indexed
	s.githubFiles = githubIndexed
	s.cachedAt = now
	s.expiresAt = now.Add(time.Duration(s.config.CacheTTLSeconds) * time.Second)
	s.lastSuccess = now
	s.stale = false
	return append([]byte(nil), body...), nil
}

type statusResponse struct {
	Status       string             `json:"status"`
	Stale        bool               `json:"stale"`
	LastAttempt  *time.Time         `json:"last_attempt_at"`
	LastSuccess  *time.Time         `json:"last_success_at"`
	CachedAt     *time.Time         `json:"cached_at"`
	CacheExpires *time.Time         `json:"cache_expires_at"`
	CachePresent bool               `json:"cache_present"`
	LastError    string             `json:"last_error,omitempty"`
	Repositories []repositoryStatus `json:"repositories"`
}

type repositoryStatus struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Owner        string `json:"owner,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Path         string `json:"path,omitempty"`
	Status       string `json:"status"`
	IndexedCount int    `json:"indexed_count"`
	SkippedCount int    `json:"skipped_count"`
	Error        string `json:"error,omitempty"`
}

func newRepositoryStatus(repository config.Repository) repositoryStatus {
	path := ""
	if repository.Type == config.LocalDirectoryType {
		path = filepath.Base(filepath.Clean(repository.Path))
	}
	return repositoryStatus{
		Name: repository.Name, Type: string(repository.Type), Owner: repository.Owner,
		Repo: repository.Repo, Branch: repository.Branch, Path: path, Status: "ok",
	}
}

func countRepositoryEntries(entries map[string]map[string]*localIndexEntry, repository string) int {
	prefix := "charts/" + repository + "/"
	count := 0
	for _, versions := range entries {
		for _, entry := range versions {
			for _, chartURL := range entry.URLs {
				if strings.HasPrefix(chartURL, prefix) {
					count++
					break
				}
			}
		}
	}
	return count
}

func mergeIndexEntries(destination, source map[string]map[string]*localIndexEntry) {
	for chartName, versions := range source {
		if destination[chartName] == nil {
			destination[chartName] = versions
			continue
		}
		for version, sourceEntry := range versions {
			destinationEntry := destination[chartName][version]
			if destinationEntry == nil {
				destination[chartName][version] = sourceEntry
				continue
			}
			for _, chartURL := range sourceEntry.URLs {
				if !containsString(destinationEntry.URLs, chartURL) {
					destinationEntry.URLs = append(destinationEntry.URLs, chartURL)
				}
			}
		}
	}
}

func sanitizeBackendError(err error, configuredPath string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if configuredPath != "" {
		message = strings.ReplaceAll(message, configuredPath, "<local-path>")
	}
	// Do not expose arbitrary absolute container paths from filesystem errors.
	message = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/)[^\s:]+`).ReplaceAllString(message, "<path>")
	return message
}

func (s *Service) status(writer http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	status := statusResponse{
		Status:       "ok",
		Stale:        s.stale,
		LastAttempt:  optionalTime(s.lastAttempt),
		LastSuccess:  optionalTime(s.lastSuccess),
		CachedAt:     optionalTime(s.cachedAt),
		CacheExpires: optionalTime(s.expiresAt),
		CachePresent: len(s.lastIndex) > 0,
		LastError:    s.lastError,
		Repositories: append([]repositoryStatus(nil), s.repositories...),
	}
	s.mu.RUnlock()
	if status.Stale {
		status.Status = "stale"
	} else if status.LastAttempt == nil {
		status.Status = "unknown"
	} else {
		for _, repository := range status.Repositories {
			if repository.Status == "failed" {
				status.Status = "partial"
				break
			}
		}
	}
	data, err := json.Marshal(status)
	if err != nil {
		writePlainError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data)
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

type localIndex struct {
	APIVersion string                       `yaml:"apiVersion"`
	Generated  string                       `yaml:"generated"`
	Entries    map[string][]localIndexEntry `yaml:"entries"`
}

type localIndexEntry struct {
	APIVersion  string            `yaml:"apiVersion,omitempty"`
	Name        string            `yaml:"name,omitempty"`
	Version     string            `yaml:"version,omitempty"`
	AppVersion  string            `yaml:"appVersion,omitempty"`
	Description string            `yaml:"description,omitempty"`
	Home        string            `yaml:"home,omitempty"`
	Icon        string            `yaml:"icon,omitempty"`
	Deprecated  *bool             `yaml:"deprecated,omitempty"`
	Keywords    []string          `yaml:"keywords,omitempty"`
	Maintainers []helmMaintainer  `yaml:"maintainers,omitempty"`
	Sources     []string          `yaml:"sources,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
	URLs        []string          `yaml:"urls,omitempty"`
	Created     string            `yaml:"created,omitempty"`
	Digest      string            `yaml:"digest,omitempty"`
}

type helmMaintainer struct {
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
	APIVersion  string            `yaml:"apiVersion,omitempty"`
	Name        string            `yaml:"name,omitempty"`
	Version     string            `yaml:"version,omitempty"`
	AppVersion  string            `yaml:"appVersion,omitempty"`
	Description string            `yaml:"description,omitempty"`
	Home        string            `yaml:"home,omitempty"`
	Icon        string            `yaml:"icon,omitempty"`
	Deprecated  *bool             `yaml:"deprecated,omitempty"`
	Keywords    []string          `yaml:"keywords,omitempty"`
	Maintainers []helmMaintainer  `yaml:"maintainers,omitempty"`
	Sources     []string          `yaml:"sources,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
	Created     string            `yaml:"created,omitempty"`
	Digest      string            `yaml:"digest,omitempty"`
	URLs        []string          `yaml:"urls"`
}

func addChartReleaserEntries(entries map[string]map[string]*localIndexEntry, repository, owner, repo string, data []byte) error {
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
			entry := entries[sourceEntry.Name][sourceEntry.Version]
			if entry == nil {
				entry = &localIndexEntry{APIVersion: sourceEntry.APIVersion, Name: sourceEntry.Name, Version: sourceEntry.Version, AppVersion: sourceEntry.AppVersion, Description: sourceEntry.Description, Home: sourceEntry.Home, Icon: sourceEntry.Icon, Deprecated: sourceEntry.Deprecated, Keywords: sourceEntry.Keywords, Maintainers: sourceEntry.Maintainers, Sources: sourceEntry.Sources, Annotations: sourceEntry.Annotations, Created: sourceEntry.Created, Digest: sourceEntry.Digest}
				if entry.APIVersion == "" {
					entry.APIVersion = "v2"
				}
				if entries[sourceEntry.Name] == nil {
					entries[sourceEntry.Name] = make(map[string]*localIndexEntry)
				}
				entries[sourceEntry.Name][sourceEntry.Version] = entry
			}
			for _, rawURL := range sourceEntry.URLs {
				rewritten, err := rewriteChartReleaserURL(rawURL, owner, repo, repository)
				if err != nil {
					return fmt.Errorf("chart %q: %w", chartName, err)
				}
				if !containsString(entry.URLs, rewritten) {
					entry.URLs = append(entry.URLs, rewritten)
				}
			}
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

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

var githubChartFilenamePattern = regexp.MustCompile(`^(.+)-v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\.tgz$`)

func parseGitHubChartFilename(filename string) (string, string, bool) {
	match := githubChartFilenamePattern.FindStringSubmatch(filename)
	if match == nil || match[1] == "" {
		return "", "", false
	}
	return match[1], match[2], true
}

func addGitHubEntry(entries map[string]map[string]*localIndexEntry, repository, name, version string, asset githubclient.Asset, created *time.Time) {
	byVersion := entries[name]
	if byVersion == nil {
		byVersion = make(map[string]*localIndexEntry)
		entries[name] = byVersion
	}
	entry := byVersion[version]
	if entry == nil {
		entry = &localIndexEntry{APIVersion: "v2", Name: name, Version: version}
		if created != nil {
			entry.Created = created.UTC().Format(time.RFC3339Nano)
		}
		if strings.HasPrefix(asset.Digest, "sha256:") {
			entry.Digest = strings.TrimPrefix(asset.Digest, "sha256:")
		}
		byVersion[version] = entry
	}
	url := "charts/" + repository + "/" + strconv.FormatInt(asset.ID, 10) + "/" + asset.Name
	for _, existing := range entry.URLs {
		if existing == url {
			return
		}
	}
	entry.URLs = append(entry.URLs, url)
}

// localcharts.Chart.Filename is the backend key for local entries. Sorting
// that key before merging keeps entries stable within each repository.
func addLocalEntry(entries map[string]map[string]*localIndexEntry, repository string, chart localcharts.Chart) {
	byVersion := entries[chart.Name]
	if byVersion == nil {
		byVersion = make(map[string]*localIndexEntry)
		entries[chart.Name] = byVersion
	}
	entry := byVersion[chart.Version]
	if entry == nil {
		entry = &localIndexEntry{
			APIVersion: "v2",
			Name:       chart.Name,
			Version:    chart.Version,
			Created:    chart.Created.UTC().Format(time.RFC3339Nano),
			Digest:     chart.Digest,
		}
		byVersion[chart.Version] = entry
	}
	url := "charts/" + repository + "/" + chart.Filename
	for _, existing := range entry.URLs {
		if existing == url {
			return
		}
	}
	entry.URLs = append(entry.URLs, url)
}

func finalizeEntries(source map[string]map[string]*localIndexEntry) map[string][]localIndexEntry {
	result := make(map[string][]localIndexEntry, len(source))
	for name, byVersion := range source {
		versions := make([]string, 0, len(byVersion))
		for version := range byVersion {
			versions = append(versions, version)
		}
		sort.SliceStable(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
		result[name] = make([]localIndexEntry, 0, len(versions))
		for _, version := range versions {
			result[name] = append(result[name], *byVersion[version])
		}
	}
	return result
}

func compareVersions(left, right string) int {
	l, lok := parseVersion(left)
	r, rok := parseVersion(right)
	if lok && rok {
		if l.major != r.major {
			return sign(l.major - r.major)
		}
		if l.minor != r.minor {
			return sign(l.minor - r.minor)
		}
		if l.patch != r.patch {
			return sign(l.patch - r.patch)
		}
		if l.pre == r.pre {
			return 0
		}
		if l.pre == "" {
			return 1
		}
		if r.pre == "" {
			return -1
		}
		return comparePrerelease(l.pre, r.pre)
	}
	if lok != rok {
		if lok {
			return 1
		}
		return -1
	}
	return strings.Compare(left, right)
}

type parsedVersion struct {
	major, minor, patch int64
	pre                 string
}

var versionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseVersion(value string) (parsedVersion, bool) {
	match := versionPattern.FindStringSubmatch(value)
	if match == nil {
		return parsedVersion{}, false
	}
	var version parsedVersion
	if _, err := fmt.Sscan(match[1], &version.major); err != nil {
		return parsedVersion{}, false
	}
	if _, err := fmt.Sscan(match[2], &version.minor); err != nil {
		return parsedVersion{}, false
	}
	if _, err := fmt.Sscan(match[3], &version.patch); err != nil {
		return parsedVersion{}, false
	}
	version.pre = match[4]
	return version, true
}

func comparePrerelease(left, right string) int {
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for i := 0; i < len(leftParts) && i < len(rightParts); i++ {
		ln, le := parseNumericIdentifier(leftParts[i])
		rn, re := parseNumericIdentifier(rightParts[i])
		if le && re && ln != rn {
			return sign(ln - rn)
		}
		if le != re {
			if le {
				return -1
			}
			return 1
		}
		if leftParts[i] != rightParts[i] {
			return strings.Compare(leftParts[i], rightParts[i])
		}
	}
	return sign(int64(len(leftParts) - len(rightParts)))
}

func parseNumericIdentifier(value string) (int64, bool) {
	var number int64
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, false
	}
	if _, err := fmt.Sscan(value, &number); err != nil {
		return 0, false
	}
	return number, true
}

func sign(value int64) int {
	if value < 0 {
		return -1
	}
	if value > 0 {
		return 1
	}
	return 0
}

func (s *Service) chartReleaser(writer http.ResponseWriter, request *http.Request) {
	repositoryName := chi.URLParam(request, "repository")
	parts, safe := safeChartReleaserPath(chi.URLParam(request, "*"))
	repository, ok := s.repository(repositoryName, config.ChartReleaserType)
	if !ok || !safe {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	backend, supported := s.github.(chartReleaserBackend)
	if !supported {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	if parts[0] != "package-in-branch" {
		tag := strings.Join(parts[:len(parts)-1], "/")
		file, err := backend.DownloadRelease(request.Context(), repository.Owner, repository.Repo, tag, parts[len(parts)-1], repository.GitHubToken)
		if err != nil {
			writePlainError(writer, http.StatusBadGateway, err.Error())
			return
		}
		streamChartPackage(writer, file, parts[len(parts)-1])
		return
	}
	file, err := backend.FetchBranchFile(request.Context(), repository.Owner, repository.Repo, repository.Branch, strings.Join(parts[1:], "/"), repository.GitHubToken)
	if err != nil {
		writePlainError(writer, http.StatusBadGateway, err.Error())
		return
	}
	streamChartPackage(writer, file, parts[len(parts)-1])
}

func (s *Service) chart(writer http.ResponseWriter, request *http.Request) {
	repositoryName := chi.URLParam(request, "repository")
	if repository, ok := s.repository(repositoryName, config.ChartReleaserType); ok {
		backend, supported := s.github.(chartReleaserBackend)
		if !supported {
			writePlainError(writer, http.StatusNotFound, "chart not found")
			return
		}
		tag, filename := chi.URLParam(request, "asset"), chi.URLParam(request, "filename")
		parts, safe := safeChartReleaserPath(tag + "/" + filename)
		if !safe || len(parts) != 2 {
			writePlainError(writer, http.StatusNotFound, "chart not found")
			return
		}
		if tag == "package-in-branch" {
			file, err := backend.FetchBranchFile(request.Context(), repository.Owner, repository.Repo, repository.Branch, filename, repository.GitHubToken)
			if err != nil {
				writePlainError(writer, http.StatusBadGateway, err.Error())
				return
			}
			streamChartPackage(writer, file, filename)
			return
		}
		file, err := backend.DownloadRelease(request.Context(), repository.Owner, repository.Repo, tag, filename, repository.GitHubToken)
		if err != nil {
			writePlainError(writer, http.StatusBadGateway, err.Error())
			return
		}
		streamChartPackage(writer, file, filename)
		return
	}
	assetID, err := strconv.ParseInt(chi.URLParam(request, "asset"), 10, 64)
	filename := chi.URLParam(request, "filename")
	if (chi.URLParam(request, "asset") != "" && (err != nil || assetID < 1)) || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\\`) || filename == "." || filename == ".." {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	s.mu.RLock()
	githubFilename, githubOK := s.githubFiles[repositoryName][assetID]
	s.mu.RUnlock()
	if githubOK {
		if filename != githubFilename {
			writePlainError(writer, http.StatusNotFound, "chart not found")
			return
		}
		repository, ok := s.repository(repositoryName, config.GitHubReleasesType)
		if !ok {
			writePlainError(writer, http.StatusNotFound, "chart not found")
			return
		}
		file, err := s.github.DownloadAsset(request.Context(), repository.Owner, repository.Repo, assetID, repository.GitHubToken)
		if err != nil {
			writePlainError(writer, http.StatusBadGateway, err.Error())
			return
		}
		defer file.Close()
		writer.Header().Set("Content-Type", "application/gzip")
		writer.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		writer.WriteHeader(http.StatusOK)
		_, _ = io.Copy(writer, file)
		return
	}
	s.mu.RLock()
	chart, ok := s.localFiles[repositoryName][filename]
	s.mu.RUnlock()
	if !ok {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	root := ""
	for _, repository := range s.config.Repositories {
		if repository.Name == repositoryName && repository.Type == config.LocalDirectoryType {
			root = repository.Path
			break
		}
	}
	root, err = filepath.Abs(root)
	if err != nil || !containedPath(root, chart.Path) {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	info, err := os.Lstat(chart.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	file, err := os.Open(chart.Path)
	if err != nil {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "application/gzip")
	writer.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, file)
}

func streamChartPackage(writer http.ResponseWriter, file io.ReadCloser, filename string) {
	defer file.Close()
	writer.Header().Set("Content-Type", "application/gzip")
	writer.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = io.Copy(writer, file)
}

func safeChartReleaserPath(rawPath string) ([]string, bool) {
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

func (s *Service) repository(name string, kind config.RepositoryType) (config.Repository, bool) {
	for _, repository := range s.config.Repositories {
		if repository.Name == name && repository.Type == kind {
			return repository, true
		}
	}
	return config.Repository{}, false
}

func containedPath(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func writePlainError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(message + "\n"))
}

func writeJSON(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(body))
}
