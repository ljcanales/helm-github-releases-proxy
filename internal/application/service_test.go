package application_test

import (
	"context"
	"errors"
	"helm-github-releases-proxy/internal/application"
	"helm-github-releases-proxy/internal/catalog"
	"helm-github-releases-proxy/internal/config"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controlledSource struct {
	contribution application.SourceContribution
	err          error
	calls        int
}

func (s *controlledSource) Discover(context.Context) (application.SourceContribution, error) {
	s.calls++
	return s.contribution, s.err
}
func (s *controlledSource) OpenPackage(_ context.Context, r application.PackageReference) (application.ChartPackage, error) {
	return application.ChartPackage{Body: io.NopCloser(strings.NewReader(r.Key)), Filename: r.Key}, nil
}
func contribution(name, key string) application.SourceContribution {
	return application.SourceContribution{IndexedCount: 1, ChartVersions: []application.DiscoveredChartVersion{{Version: catalog.ChartVersion{Name: name, Version: "1.0.0", URLs: []string{"charts/local/" + key}}, Packages: []application.DiscoveredPackage{{URL: "charts/local/" + key, Reference: application.PackageReference{Source: "local", Key: key}}}}}}
}
func TestRefreshRetainsEntirePublicationOnFailure(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	local := &controlledSource{contribution: contribution("old", "old.tgz")}
	upstream := &controlledSource{}
	s := application.New([]application.Source{{Repository: config.Repository{Name: "upstream", Type: config.GitHubReleasesType}, Discovery: upstream, Packages: upstream}, {Repository: config.Repository{Name: "local", Type: config.LocalDirectoryType}, Discovery: local, Packages: local}}, time.Minute, func() time.Time { return now }, context.Background())
	first, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	s.Index()
	if local.calls != 1 {
		t.Fatal("fresh index rebuilt")
	}
	now = now.Add(time.Minute)
	local.contribution = contribution("new", "new.tgz")
	upstream.err = errors.New("cannot read /private/token")
	fallback, err := s.Index()
	if err != nil || !reflect.DeepEqual(fallback, first) {
		t.Fatal("failed refresh replaced aggregate")
	}
	if _, err := s.OpenPackage(context.Background(), "local", "", "new.tgz"); !errors.Is(err, application.ErrPackageNotFound) {
		t.Fatal("new package published during failure")
	}
	pkg, err := s.OpenPackage(context.Background(), "local", "", "old.tgz")
	if err != nil {
		t.Fatal(err)
	}
	pkg.Body.Close()
	status := s.Status()
	if status.Status != "stale" || status.LastSuccess == nil || strings.Contains(status.Repositories[0].Error, "/private") || status.Repositories[1].IndexedCount != 1 {
		t.Fatalf("status: %+v", status)
	}
	upstream.err = nil
	recovered, err := s.Index()
	if err != nil || len(recovered.Entries["new"]) != 1 {
		t.Fatal("successful refresh did not publish")
	}
	pkg, err = s.OpenPackage(context.Background(), "local", "", "new.tgz")
	if err != nil {
		t.Fatal(err)
	}
	pkg.Body.Close()
}

func TestPartialBecomesStaleAndZeroTTLAlwaysRebuilds(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	local := &controlledSource{contribution: contribution("demo", "demo.tgz")}
	broken := &controlledSource{err: errors.New("unavailable")}
	s := application.New([]application.Source{{Repository: config.Repository{Name: "local", Type: config.LocalDirectoryType}, Discovery: local, Packages: local}, {Repository: config.Repository{Name: "remote", Type: config.GitHubReleasesType}, Discovery: broken, Packages: broken}}, 0, func() time.Time { return now }, context.Background())
	if s.Status().Status != "unknown" {
		t.Fatal("initial status")
	}
	first, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	status := s.Status()
	if status.Status != "partial" || !status.CachePresent || status.CacheExpires != nil || status.LastSuccess != nil || status.CachedAt != nil {
		t.Fatalf("partial: %+v", status)
	}
	now = now.Add(time.Second)
	second, err := s.Index()
	if err != nil || !reflect.DeepEqual(first, second) || s.Status().Status != "stale" || local.calls != 2 {
		t.Fatal("partial was not retained and retried")
	}
	broken.err = nil
	s.Index()
	status = s.Status()
	if status.Status != "ok" || status.LastSuccess == nil || !status.LastAttempt.Equal(now) {
		t.Fatalf("recovered: %+v", status)
	}
	s.Index()
	if local.calls != 4 {
		t.Fatal("zero TTL reused successful refresh")
	}
}

type blockingSource struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	next    atomic.Bool
}

func (s *blockingSource) Discover(ctx context.Context) (application.SourceContribution, error) {
	if ctx.Value(contextKey{}) != "refresh" {
		return application.SourceContribution{}, errors.New("wrong refresh context")
	}
	n := s.calls.Add(1)
	if n == 2 {
		close(s.started)
		<-s.release
	}
	if s.next.Load() {
		return contribution("new", "new.tgz"), nil
	}
	return contribution("old", "old.tgz"), nil
}
func (s *blockingSource) OpenPackage(_ context.Context, r application.PackageReference) (application.ChartPackage, error) {
	return application.ChartPackage{Body: io.NopCloser(strings.NewReader(r.Key)), Filename: r.Key}, nil
}

type contextKey struct{}

func TestConcurrentRefreshReusesSuccessAndPublishesLookupsTogether(t *testing.T) {
	var seconds atomic.Int64
	source := &blockingSource{started: make(chan struct{}), release: make(chan struct{})}
	s := application.New([]application.Source{{Repository: config.Repository{Name: "local", Type: config.LocalDirectoryType}, Discovery: source, Packages: source}}, time.Minute, func() time.Time { return time.Unix(seconds.Load(), 0) }, context.WithValue(context.Background(), contextKey{}, "refresh"))
	s.Index()
	seconds.Store(60)
	source.next.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); s.Index() }()
	<-source.started
	pkg, err := s.OpenPackage(context.Background(), "local", "", "old.tgz")
	if err != nil {
		t.Fatal(err)
	}
	pkg.Body.Close()
	if _, err := s.OpenPackage(context.Background(), "local", "", "new.tgz"); !errors.Is(err, application.ErrPackageNotFound) {
		t.Fatal("lookup changed before aggregate publication")
	}
	if !s.Status().LastAttempt.Equal(time.Unix(0, 0)) {
		t.Fatal("attempt published before completion")
	}
	var wait sync.WaitGroup
	ready := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ready <- struct{}{}
			body, err := s.Index()
			if err != nil || len(body.Entries["new"]) != 1 {
				t.Error("waiter did not receive new aggregate")
			}
			pkg, err := s.OpenPackage(context.Background(), "local", "", "new.tgz")
			if err != nil {
				t.Error(err)
			} else {
				pkg.Body.Close()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-ready
	}
	close(source.release)
	<-done
	wait.Wait()
	if source.calls.Load() != 2 {
		t.Fatalf("successful refresh not reused: %d", source.calls.Load())
	}
}

func TestPackageEligibilityIsSourceSpecific(t *testing.T) {
	local := &controlledSource{contribution: contribution("demo", "demo.tgz")}
	github := &controlledSource{contribution: application.SourceContribution{ChartVersions: []application.DiscoveredChartVersion{{Version: catalog.ChartVersion{Name: "remote", Version: "1.0.0"}, Packages: []application.DiscoveredPackage{{URL: "charts/github/42/demo.tgz", Reference: application.PackageReference{Source: "github", Key: "opaque"}}}}}}}
	direct := &controlledSource{}
	s := application.New([]application.Source{{Repository: config.Repository{Name: "local", Type: config.LocalDirectoryType}, Discovery: local, Packages: local}, {Repository: config.Repository{Name: "github", Type: config.GitHubReleasesType}, Discovery: github, Packages: github}, {Repository: config.Repository{Name: "pages", Type: config.ChartReleaserType}, Discovery: direct, Packages: direct}}, time.Minute, time.Now, context.Background())
	pkg, err := s.OpenDirectPackage(context.Background(), "pages", "tag/unindexed.tgz")
	if err != nil {
		t.Fatal(err)
	}
	pkg.Body.Close()
	if direct.calls != 0 {
		t.Fatal("direct download acquired index")
	}
	s.Index()
	for _, test := range []struct {
		name, asset, file string
		valid             bool
	}{{"local", "", "demo.tgz", true}, {"local", "", "../demo.tgz", false}, {"local", "", "unknown.tgz", false}, {"github", "42", "demo.tgz", true}, {"github", "42", "wrong.tgz", false}, {"github", "43", "demo.tgz", false}, {"github", "", "demo.tgz", false}, {"pages", "tag", "unindexed.tgz", true}, {"pages", "", "tag%2Funindexed.tgz", false}, {"pages", "tag", "nested%2Funindexed.tgz", false}, {"pages", "tag", "nested%252Funindexed.tgz", false}} {
		pkg, err := s.OpenPackage(context.Background(), test.name, test.asset, test.file)
		if test.valid {
			if err != nil {
				t.Fatal(err)
			}
			pkg.Body.Close()
		} else if !errors.Is(err, application.ErrPackageNotFound) {
			t.Fatalf("unexpected eligibility: %+v %v", test, err)
		}
	}
	for _, name := range []string{"local", "github", "unknown"} {
		if _, err := s.OpenDirectPackage(context.Background(), name, "tag/unindexed.tgz"); !errors.Is(err, application.ErrPackageNotFound) {
			t.Fatalf("direct download from %s: %v", name, err)
		}
	}

}

type failingSource struct {
	calls            atomic.Int32
	started, release chan struct{}
}

func (s *failingSource) Discover(context.Context) (application.SourceContribution, error) {
	if s.calls.Add(1) == 1 {
		close(s.started)
		<-s.release
	}
	return application.SourceContribution{}, errors.New("unavailable")
}
func TestConcurrentCallersRepeatFailedRefreshAttempts(t *testing.T) {
	source := &failingSource{started: make(chan struct{}), release: make(chan struct{})}
	s := application.New([]application.Source{{Repository: config.Repository{Name: "remote", Type: config.GitHubReleasesType}, Discovery: source}}, time.Minute, time.Now, context.Background())
	var wait sync.WaitGroup
	results := make(chan application.Index, 5)
	call := func() {
		defer wait.Done()
		index, err := s.Index()
		if err != nil {
			t.Error(err)
		}
		results <- index
	}
	wait.Add(1)
	go call()
	<-source.started
	ready := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() { ready <- struct{}{}; call() }()
	}
	for i := 0; i < 4; i++ {
		<-ready
	}
	close(source.release)
	wait.Wait()
	close(results)
	if source.calls.Load() != 5 {
		t.Fatalf("failed results shared among callers: %d attempts", source.calls.Load())
	}
	var first *application.Index
	for index := range results {
		if first == nil {
			copy := index
			first = &copy
		} else if !reflect.DeepEqual(*first, index) {
			t.Fatal("retained partial aggregate changed")
		}
	}
	if s.Status().Status != "stale" || s.Status().CacheExpires != nil {
		t.Fatalf("failure status: %+v", s.Status())
	}
}

func TestStartupWarmingIsAsynchronousAndOnce(t *testing.T) {
	source := &failingSource{started: make(chan struct{}), release: make(chan struct{})}
	s := application.New([]application.Source{{Repository: config.Repository{Name: "remote", Type: config.GitHubReleasesType}, Discovery: source}}, time.Minute, time.Now, context.Background())
	returned := make(chan struct{})
	go func() { s.Start(nil); s.Start(nil); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("startup waited for acquisition")
	}
	<-source.started
	if s.Status().Status != "unknown" {
		t.Fatal("incomplete warm published status")
	}
	close(source.release)
	// An index caller serializes behind warming and retries its failed acquisition.
	s.Index()
	if source.calls.Load() != 2 || s.Status().Status != "stale" {
		t.Fatal("warming repeated or failed attempt was shared")
	}
}

func TestAggregateSnapshotsCannotMutateServedState(t *testing.T) {
	local := &controlledSource{contribution: contribution("demo", "demo.tgz")}
	local.contribution.ChartVersions[0].Version.Annotations = map[string]string{"owner": "original"}
	s := application.New([]application.Source{{Repository: config.Repository{Name: "local", Type: config.LocalDirectoryType}, Discovery: local, Packages: local}}, time.Minute, time.Now, context.Background())
	index, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	index.Entries["demo"][0].URLs[0] = "changed"
	index.Entries["demo"][0].Annotations["owner"] = "changed"
	local.contribution.ChartVersions[0].Version.Annotations["owner"] = "source changed"
	again, _ := s.Index()
	if again.Entries["demo"][0].URLs[0] != "charts/local/demo.tgz" || again.Entries["demo"][0].Annotations["owner"] != "original" {
		t.Fatal("external mutation changed publication")
	}
}
