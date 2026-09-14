package application

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"helm-github-releases-proxy/internal/catalog"
	"helm-github-releases-proxy/internal/config"
)

// Source binds configured discovery and package access without owning a cache.
type Source struct {
	Repository config.Repository
	Discovery  ChartDiscovery
	Packages   PackageOpener
}

// Service owns the aggregate and its package lookups as a single publication.
type Service struct {
	sources                                       []Source
	ttl                                           time.Duration
	now                                           func() time.Time
	refreshContext                                context.Context
	mu                                            sync.RWMutex
	buildMu                                       sync.Mutex
	warmOnce                                      sync.Once
	localFiles                                    map[string]map[string]PackageReference
	githubFiles                                   map[string]map[int64]githubPackage
	lastIndex                                     *Index
	cachedAt, expiresAt, lastAttempt, lastSuccess time.Time
	lastError                                     string
	stale                                         bool
	repositories                                  []RepositoryStatus
}
type githubPackage struct {
	filename  string
	reference PackageReference
}

func New(sources []Source, ttl time.Duration, now func() time.Time, refreshContext context.Context) *Service {
	s := &Service{sources: append([]Source(nil), sources...), ttl: ttl, now: now, refreshContext: refreshContext}
	for _, source := range sources {
		s.repositories = append(s.repositories, newRepositoryStatus(source.Repository))
	}
	return s
}

// Start warms once asynchronously; acquisition uses the configured refresh lifetime.
func (s *Service) Start(onError func(error)) {
	s.warmOnce.Do(func() {
		go func() {
			_, err := s.Index()
			if err != nil && onError != nil {
				onError(err)
			}
		}()
	})
}

// Index reuses a fresh successful publication or performs a serialized refresh.
func (s *Service) Index() (Index, error) { return s.refresh() }

var ErrPackageNotFound = errors.New("chart not found")

// OpenPackage resolves a filename and optional asset or tag, then opens outside
// the lock. Release and local packages require published lookup membership;
// chart-releaser packages require a two-segment reference validated by their source.
func (s *Service) OpenPackage(ctx context.Context, name, asset, filename string) (ChartPackage, error) {
	for _, source := range s.sources {
		if source.Repository.Name != name {
			continue
		}
		if source.Repository.Type != config.ChartReleaserType && (filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) || filename == "." || filename == "..") {
			return ChartPackage{}, ErrPackageNotFound
		}
		var reference PackageReference
		switch source.Repository.Type {
		case config.ChartReleaserType:
			if asset == "" {
				return ChartPackage{}, ErrPackageNotFound
			}
			key := asset + "/" + filename
			// The two-parameter route accepts exactly two decoded segments.
			// Direct wildcard routes retain support for nested package paths.
			decoded := key
			for {
				next, err := url.PathUnescape(decoded)
				if err != nil {
					return ChartPackage{}, ErrPackageNotFound
				}
				if next == decoded {
					break
				}
				decoded = next
			}
			if len(strings.Split(decoded, "/")) != 2 {
				return ChartPackage{}, ErrPackageNotFound
			}
			reference = PackageReference{Source: name, Key: key}
		case config.GitHubReleasesType:
			id, err := strconv.ParseInt(asset, 10, 64)
			if err != nil || id < 1 {
				return ChartPackage{}, ErrPackageNotFound
			}
			s.mu.RLock()
			pkg, ok := s.githubFiles[name][id]
			s.mu.RUnlock()
			if !ok || pkg.filename != filename {
				return ChartPackage{}, ErrPackageNotFound
			}
			reference = pkg.reference
		case config.LocalDirectoryType:
			if asset != "" {
				id, err := strconv.ParseInt(asset, 10, 64)
				if err != nil || id < 1 {
					return ChartPackage{}, ErrPackageNotFound
				}
			}
			s.mu.RLock()
			ref, ok := s.localFiles[name][filename]
			s.mu.RUnlock()
			if !ok {
				return ChartPackage{}, ErrPackageNotFound
			}
			reference = ref
		default:
			return ChartPackage{}, ErrPackageNotFound
		}
		pkg, err := source.Packages.OpenPackage(ctx, reference)
		if err != nil && source.Repository.Type == config.LocalDirectoryType {
			return ChartPackage{}, ErrPackageNotFound
		}
		return pkg, err
	}
	return ChartPackage{}, ErrPackageNotFound
}

// OpenDirectPackage opens a chart-releaser path without index membership.
// Other source modes require published package lookup and reject direct paths.
func (s *Service) OpenDirectPackage(ctx context.Context, name, key string) (ChartPackage, error) {
	for _, source := range s.sources {
		if source.Repository.Name == name && source.Repository.Type == config.ChartReleaserType {
			return source.Packages.OpenPackage(ctx, PackageReference{Source: name, Key: key})
		}
	}
	return ChartPackage{}, ErrPackageNotFound
}

func (s *Service) refresh() (Index, error) {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	now := s.now().UTC()
	s.mu.RLock()
	if s.lastIndex != nil && !s.expiresAt.IsZero() && now.Before(s.expiresAt) {
		body := cloneIndex(*s.lastIndex)
		s.mu.RUnlock()
		return body, nil
	}
	s.mu.RUnlock()

	entries := make(catalog.AggregateIndex)
	indexed := make(map[string]map[string]PackageReference)
	githubIndexed := make(map[string]map[int64]githubPackage)
	failedRepositories := make([]string, 0)
	repositoryStatuses := make([]RepositoryStatus, 0, len(s.sources))
	for _, source := range s.sources {
		repository := source.Repository
		sourceStatus := newRepositoryStatus(repository)
		contribution, err := source.Discovery.Discover(s.refreshContext)
		if err != nil {
			failedRepositories = append(failedRepositories, repository.Name)
			sourceStatus.Status = "failed"
			sourceStatus.Error = sanitizeBackendError(err, repository.Path)
		} else {
			sourceStatus.IndexedCount = contribution.IndexedCount
			sourceStatus.SkippedCount = contribution.SkippedCount
			for _, chart := range contribution.ChartVersions {
				entries.Add(chart.Version)
				for _, pkg := range chart.Packages {
					switch repository.Type {
					case config.LocalDirectoryType:
						if indexed[repository.Name] == nil {
							indexed[repository.Name] = make(map[string]PackageReference)
						}
						indexed[repository.Name][pkg.Reference.Key] = pkg.Reference
					case config.GitHubReleasesType:
						if githubIndexed[repository.Name] == nil {
							githubIndexed[repository.Name] = make(map[int64]githubPackage)
						}
						route := strings.TrimPrefix(pkg.URL, "charts/"+repository.Name+"/")
						id, filename, _ := strings.Cut(route, "/")
						assetID, _ := strconv.ParseInt(id, 10, 64)
						githubIndexed[repository.Name][assetID] = githubPackage{filename: filename, reference: pkg.Reference}
					}
				}
			}
		}
		repositoryStatuses = append(repositoryStatuses, sourceStatus)
	}
	body := Index{Generated: now, Entries: entries.Finalize()}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = now
	s.lastError = ""
	s.repositories = repositoryStatuses
	if len(failedRepositories) > 0 {
		s.lastError = "repositories failed: " + strings.Join(failedRepositories, ", ")
		if s.lastIndex != nil {
			body = cloneIndex(*s.lastIndex)
			s.stale = true
		} else {
			// A partial build is still the best available aggregate when no
			// previous index exists. Keep it for later stale fallback, but do
			// not give the failed attempt a fresh TTL.
			published := cloneIndex(body)
			s.lastIndex = &published
			s.localFiles = indexed
			s.githubFiles = githubIndexed
			s.stale = false
		}
		return body, nil
	}

	published := cloneIndex(body)
	s.lastIndex = &published
	s.localFiles = indexed
	s.githubFiles = githubIndexed
	s.cachedAt = now
	s.expiresAt = now.Add(s.ttl)
	s.lastSuccess = now
	s.stale = false
	return cloneIndex(body), nil
}

type Status struct {
	Status       string
	Stale        bool
	LastAttempt  *time.Time
	LastSuccess  *time.Time
	CachedAt     *time.Time
	CacheExpires *time.Time
	CachePresent bool
	LastError    string
	Repositories []RepositoryStatus
}

type RepositoryStatus struct {
	Name         string
	Type         string
	Owner        string
	Repo         string
	Branch       string
	Path         string
	Status       string
	IndexedCount int
	SkippedCount int
	Error        string
}

func newRepositoryStatus(repository config.Repository) RepositoryStatus {
	path := ""
	if repository.Type == config.LocalDirectoryType {
		path = filepath.Base(filepath.Clean(repository.Path))
	}
	return RepositoryStatus{
		Name: repository.Name, Type: string(repository.Type), Owner: repository.Owner,
		Repo: repository.Repo, Branch: repository.Branch, Path: path, Status: "ok",
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

func (s *Service) Status() Status {
	s.mu.RLock()
	status := Status{
		Status:       "ok",
		Stale:        s.stale,
		LastAttempt:  optionalTime(s.lastAttempt),
		LastSuccess:  optionalTime(s.lastSuccess),
		CachedAt:     optionalTime(s.cachedAt),
		CacheExpires: optionalTime(s.expiresAt),
		CachePresent: s.lastIndex != nil,
		LastError:    s.lastError,
		Repositories: append([]RepositoryStatus(nil), s.repositories...),
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
	return status
}
func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

// Index is the retained aggregate, independent of its HTTP representation.
type Index struct {
	Generated time.Time
	Entries   map[string][]catalog.ChartVersion
}

// cloneIndex isolates published state from source contributions and callers.
func cloneIndex(index Index) Index {
	result := Index{Generated: index.Generated, Entries: make(map[string][]catalog.ChartVersion, len(index.Entries))}
	for name, versions := range index.Entries {
		result.Entries[name] = make([]catalog.ChartVersion, len(versions))
		for i, version := range versions {
			version.URLs = append([]string(nil), version.URLs...)
			version.Keywords = append([]string(nil), version.Keywords...)
			version.Sources = append([]string(nil), version.Sources...)
			version.Maintainers = append([]catalog.Maintainer(nil), version.Maintainers...)
			if version.Deprecated != nil {
				deprecated := *version.Deprecated
				version.Deprecated = &deprecated
			}
			if version.Annotations != nil {
				annotations := make(map[string]string, len(version.Annotations))
				for key, value := range version.Annotations {
					annotations[key] = value
				}
				version.Annotations = annotations
			}
			result.Entries[name][i] = version
		}
	}
	return result
}
