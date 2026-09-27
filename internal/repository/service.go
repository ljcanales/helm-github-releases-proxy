package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"helm-github-releases-proxy/internal/chart"
)

var ErrPackageNotFound = errors.New("chart not found")

type Option func(*Service)

func WithTTL(ttl time.Duration) Option { return func(service *Service) { service.ttl = ttl } }

func WithClock(now func() time.Time) Option { return func(service *Service) { service.now = now } }

func WithRefreshContext(ctx context.Context) Option {
	return func(service *Service) { service.refreshContext = ctx }
}

type publication struct {
	index  Index
	lookup map[string]publishedPackage
}

type publishedPackage struct {
	sourceIndex int
	reference   PackageReference
}

type Service struct {
	sources        Sources
	ttl            time.Duration
	now            func() time.Time
	refreshContext context.Context

	mu      sync.RWMutex
	buildMu sync.Mutex
	warm    sync.Once

	published                                     *publication
	cachedAt, expiresAt, lastAttempt, lastSuccess time.Time
	lastError                                     string
	stale                                         bool
	sourceStatuses                                []SourceStatus
}

func New(sources Sources, options ...Option) *Service {
	service := &Service{
		sources:        append(Sources(nil), sources...),
		ttl:            time.Minute,
		now:            time.Now,
		refreshContext: context.Background(),
	}
	for _, option := range options {
		option(service)
	}
	if service.now == nil {
		service.now = time.Now
	}
	if service.refreshContext == nil {
		service.refreshContext = context.Background()
	}
	for _, source := range service.sources {
		service.sourceStatuses = append(service.sourceStatuses, newSourceStatus(source.Config))
	}
	return service
}

func (service *Service) Start(onError func(error)) {
	service.warm.Do(func() {
		go func() {
			_, err := service.Index()
			if err != nil && onError != nil {
				onError(err)
			}
		}()
	})
}

func (service *Service) Index() (Index, error) { return service.refresh() }

// OpenPackage performs an exact lookup using the escaped path advertised in the
// index. It never triggers or waits for discovery, and opens bytes after releasing
// the publication lock.
func (service *Service) OpenPackage(ctx context.Context, sourceName, advertisedPath string) (ChartPackage, error) {
	service.mu.RLock()
	if service.published == nil {
		service.mu.RUnlock()
		return ChartPackage{}, ErrPackageNotFound
	}
	sourceIndex := -1
	for index := range service.sources {
		if service.sources[index].Config.Name == sourceName {
			sourceIndex = index
			break
		}
	}
	if sourceIndex < 0 {
		service.mu.RUnlock()
		return ChartPackage{}, ErrPackageNotFound
	}
	source := service.sources[sourceIndex]
	published, found := service.published.lookup[advertisedPath]
	if found && published.sourceIndex != sourceIndex {
		found = false
	}
	if !found {
		service.mu.RUnlock()
		return ChartPackage{}, ErrPackageNotFound
	}
	reference := published.reference
	service.mu.RUnlock()

	if source.Packages == nil {
		return ChartPackage{}, ErrPackageNotFound
	}
	pkg, err := source.Packages.OpenPackage(ctx, reference)
	if errors.Is(err, ErrPackageNotFound) {
		return ChartPackage{}, ErrPackageNotFound
	}
	return pkg, err
}

func (service *Service) refresh() (Index, error) {
	service.buildMu.Lock()
	defer service.buildMu.Unlock()

	now := service.now().UTC()
	service.mu.RLock()
	if service.published != nil && !service.expiresAt.IsZero() && now.Before(service.expiresAt) {
		index := cloneIndex(service.published.index)
		service.mu.RUnlock()
		return index, nil
	}
	service.mu.RUnlock()

	aggregate := make(chart.Aggregate)
	lookup := make(map[string]publishedPackage)
	statuses := make([]SourceStatus, 0, len(service.sources))
	failed := make([]string, 0)
	for sourceIndex, source := range service.sources {
		status := newSourceStatus(source.Config)
		contribution, err := source.Discovery.Discover(service.refreshContext)
		if err != nil {
			status.Status = "failed"
			status.Error = sanitizeError(err, source.Config.Path)
			failed = append(failed, source.Config.Name)
			statuses = append(statuses, status)
			continue
		}
		status.IndexedCount = contribution.IndexedCount
		status.SkippedCount = contribution.SkippedCount
		for _, discovered := range contribution.ChartVersions {
			version := discovered.Version
			version.URLs = make([]string, 0, len(discovered.Packages))
			for _, pkg := range discovered.Packages {
				version.URLs = append(version.URLs, pkg.AdvertisedPath)
				if pkg.AdvertisedPath != "" {
					if _, exists := lookup[pkg.AdvertisedPath]; !exists {
						lookup[pkg.AdvertisedPath] = publishedPackage{sourceIndex: sourceIndex, reference: pkg.Reference}
					}
				}
			}
			aggregate.Add(version)
		}
		statuses = append(statuses, status)
	}
	candidate := publication{index: Index{Generated: now, Entries: aggregate.Finalize()}, lookup: lookup}

	service.mu.Lock()
	defer service.mu.Unlock()
	service.lastAttempt = now
	service.sourceStatuses = statuses
	service.lastError = ""
	if len(failed) > 0 {
		service.lastError = "repositories failed: " + strings.Join(failed, ", ")
		if service.published != nil {
			service.stale = true
			return cloneIndex(service.published.index), nil
		}
		service.published = clonePublication(candidate)
		service.stale = false
		return cloneIndex(candidate.index), nil
	}

	service.published = clonePublication(candidate)
	service.cachedAt = now
	service.expiresAt = now.Add(service.ttl)
	service.lastSuccess = now
	service.stale = false
	return cloneIndex(candidate.index), nil
}

type Index struct {
	Generated time.Time
	Entries   map[string][]chart.Version
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
	Repositories []SourceStatus
}

type SourceStatus struct {
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

func (service *Service) Status() Status {
	service.mu.RLock()
	status := Status{Status: "ok", Stale: service.stale, LastAttempt: optionalTime(service.lastAttempt), LastSuccess: optionalTime(service.lastSuccess), CachedAt: optionalTime(service.cachedAt), CacheExpires: optionalTime(service.expiresAt), CachePresent: service.published != nil, LastError: service.lastError, Repositories: append([]SourceStatus(nil), service.sourceStatuses...)}
	service.mu.RUnlock()
	if status.Stale {
		status.Status = "stale"
	} else if status.LastAttempt == nil {
		status.Status = "unknown"
	} else {
		for _, source := range status.Repositories {
			if source.Status == "failed" {
				status.Status = "partial"
				break
			}
		}
	}
	return status
}

func newSourceStatus(config SourceConfig) SourceStatus {
	return SourceStatus{Name: config.Name, Type: string(config.Kind), Owner: config.Owner, Repo: config.Repo, Branch: config.Branch, Path: config.Path, Status: "ok"}
}

func sanitizeError(err error, configuredPath string) string {
	message := err.Error()
	if configuredPath != "" {
		message = strings.ReplaceAll(message, configuredPath, "<local-path>")
	}
	return regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/)[^\s:]+`).ReplaceAllString(message, "<path>")
}

func clonePublication(source publication) *publication {
	result := &publication{index: cloneIndex(source.index), lookup: make(map[string]publishedPackage, len(source.lookup))}
	for path, pkg := range source.lookup {
		result.lookup[path] = pkg
	}
	return result
}

func cloneIndex(source Index) Index {
	result := Index{Generated: source.Generated, Entries: make(map[string][]chart.Version, len(source.Entries))}
	for name, versions := range source.Entries {
		result.Entries[name] = make([]chart.Version, len(versions))
		for index, version := range versions {
			copy := version
			copy.URLs = append([]string(nil), version.URLs...)
			copy.Keywords = append([]string(nil), version.Keywords...)
			copy.Sources = append([]string(nil), version.Sources...)
			copy.Maintainers = append([]chart.Maintainer(nil), version.Maintainers...)
			if version.Deprecated != nil {
				deprecated := *version.Deprecated
				copy.Deprecated = &deprecated
			}
			if version.Annotations != nil {
				copy.Annotations = make(map[string]string, len(version.Annotations))
				for key, value := range version.Annotations {
					copy.Annotations[key] = value
				}
			}
			result.Entries[name][index] = copy
		}
	}
	return result
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
