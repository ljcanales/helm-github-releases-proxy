package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"helm-github-releases-proxy/internal/logging"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"helm-github-releases-proxy/internal/chart"
	"helm-github-releases-proxy/internal/repository"
)

func TestPublicationMatchesOnlyExplicitAdvertisedPaths(t *testing.T) {
	opener := &recordingOpener{}
	source := repository.Source{
		Config: repository.SourceConfig{Name: "assembled", Kind: "test"},
		Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
			return contribution("widget", "1.0.0",
				packageAt("charts/assembled/nested/release%2Bbuild/widget-1.0.0.tgz", "opaque-one"),
				packageAt("charts/assembled/other/widget-1.0.0.tgz", "opaque-two"),
			), nil
		}),
		Packages: opener,
	}
	service := repository.New(repository.Sources{source}, repository.WithClock(fixedClock), repository.WithTTL(time.Minute))

	index, err := service.Index()
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if got := index.Entries["widget"][0].URLs; len(got) != 2 || got[0] != "charts/assembled/nested/release%2Bbuild/widget-1.0.0.tgz" {
		t.Fatalf("advertised URLs = %#v", got)
	}

	pkg, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/nested/release%2Bbuild/widget-1.0.0.tgz")
	if err != nil {
		t.Fatalf("OpenPackage(advertised) error = %v", err)
	}
	defer pkg.Body.Close()
	if body, _ := io.ReadAll(pkg.Body); string(body) != "opaque-one" {
		t.Fatalf("package body = %q", body)
	}

	for _, path := range []string{
		"charts/assembled/nested/release+build/widget-1.0.0.tgz",
		"charts/assembled/nested%2Frelease%2Bbuild/widget-1.0.0.tgz",
		"charts/assembled/nested/release%252Bbuild/widget-1.0.0.tgz",
		"charts/assembled/other/../nested/release%2Bbuild/widget-1.0.0.tgz",
	} {
		if _, err := service.OpenPackage(context.Background(), "assembled", path); !errors.Is(err, repository.ErrPackageNotFound) {
			t.Errorf("OpenPackage(%q) error = %v, want not found", path, err)
		}
	}
	if opener.Calls() != 1 {
		t.Fatalf("opener calls = %d, want 1", opener.Calls())
	}
}

func TestDownloadBeforeInitialPublicationDoesNotWaitOrDiscover(t *testing.T) {
	discoveryStarted := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	var once sync.Once
	discovery := discoveryFunc(func(context.Context) (repository.Contribution, error) {
		once.Do(func() { close(discoveryStarted) })
		<-releaseDiscovery
		return contribution("widget", "1.0.0", packageAt("charts/assembled/widget.tgz", "ref")), nil
	})
	opener := &recordingOpener{}
	service := repository.New(repository.Sources{{Config: repository.SourceConfig{Name: "assembled", Kind: "test"}, Discovery: discovery, Packages: opener}})

	indexDone := make(chan struct{})
	go func() {
		_, _ = service.Index()
		close(indexDone)
	}()
	<-discoveryStarted

	result := make(chan error, 1)
	go func() {
		_, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/widget.tgz")
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, repository.ErrPackageNotFound) {
			t.Fatalf("OpenPackage() error = %v, want not found", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenPackage waited for initial discovery")
	}
	if opener.Calls() != 0 {
		t.Fatalf("opener calls = %d, want 0", opener.Calls())
	}
	close(releaseDiscovery)
	<-indexDone
}

func TestPublicationAndLookupMoveTogetherAcrossFallbackAndReplacement(t *testing.T) {
	attempt := 0
	discovery := discoveryFunc(func(context.Context) (repository.Contribution, error) {
		attempt++
		switch attempt {
		case 1:
			return contribution("widget", "1.0.0", packageAt("charts/assembled/old.tgz", "old")), nil
		case 2:
			return contribution("widget", "2.0.0", packageAt("charts/assembled/unpublished.tgz", "unpublished")), errors.New("upstream unavailable")
		default:
			return contribution("widget", "3.0.0", packageAt("charts/assembled/new.tgz", "new")), nil
		}
	})
	opener := &recordingOpener{}
	service := repository.New(repository.Sources{{Config: repository.SourceConfig{Name: "assembled", Kind: "test"}, Discovery: discovery, Packages: opener}}, repository.WithTTL(0))

	if _, err := service.Index(); err != nil {
		t.Fatal(err)
	}
	stale, err := service.Index()
	if err != nil {
		t.Fatal(err)
	}
	if stale.Entries["widget"][0].Version != "1.0.0" {
		t.Fatalf("stale version = %q", stale.Entries["widget"][0].Version)
	}
	if _, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/old.tgz"); err != nil {
		t.Fatalf("retained package error = %v", err)
	}
	if _, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/unpublished.tgz"); !errors.Is(err, repository.ErrPackageNotFound) {
		t.Fatalf("failed-attempt package error = %v", err)
	}

	if _, err := service.Index(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/old.tgz"); !errors.Is(err, repository.ErrPackageNotFound) {
		t.Fatalf("replaced package error = %v", err)
	}
	if _, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/new.tgz"); err != nil {
		t.Fatalf("new package error = %v", err)
	}
}

func TestPendingLaterRefreshKeepsTheRetainedPublicationAvailable(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	attempts := 0
	discovery := discoveryFunc(func(context.Context) (repository.Contribution, error) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt == 2 {
			close(started)
			<-release
			return contribution("widget", "2.0.0", packageAt("charts/assembled/new.tgz", "new")), nil
		}
		return contribution("widget", "1.0.0", packageAt("charts/assembled/old.tgz", "old")), nil
	})
	opener := &recordingOpener{}
	service := repository.New(repository.Sources{{Config: repository.SourceConfig{Name: "assembled", Kind: "test"}, Discovery: discovery, Packages: opener}}, repository.WithTTL(0))
	if _, err := service.Index(); err != nil {
		t.Fatal(err)
	}

	refreshDone := make(chan struct{})
	go func() {
		_, _ = service.Index()
		close(refreshDone)
	}()
	<-started

	openDone := make(chan error, 1)
	go func() {
		pkg, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/old.tgz")
		if pkg.Body != nil {
			pkg.Body.Close()
		}
		openDone <- err
	}()
	select {
	case err := <-openDone:
		if err != nil {
			t.Fatalf("retained package error while refresh pending = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lookup waited for later discovery")
	}

	close(release)
	<-refreshDone
	if _, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/old.tgz"); !errors.Is(err, repository.ErrPackageNotFound) {
		t.Fatalf("old package after replacement = %v, want not found", err)
	}
	if _, err := service.OpenPackage(context.Background(), "assembled", "charts/assembled/new.tgz"); err != nil {
		t.Fatalf("new package after replacement = %v", err)
	}
}

func TestInFlightDownloadKeepsItsPublicationReference(t *testing.T) {
	path := "charts/assembled/widget.tgz"
	attempt := 0
	discovery := discoveryFunc(func(context.Context) (repository.Contribution, error) {
		attempt++
		key := "old"
		version := "1.0.0"
		if attempt == 2 {
			key = "new"
			version = "2.0.0"
		}
		return contribution("widget", version, packageAt(path, key)), nil
	})
	opener := &rotatingOpener{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	service := repository.New(repository.Sources{{Config: repository.SourceConfig{Name: "assembled", Kind: "test"}, Discovery: discovery, Packages: opener}}, repository.WithTTL(0))
	if _, err := service.Index(); err != nil {
		t.Fatal(err)
	}

	firstDownload := make(chan string, 1)
	go func() {
		pkg, err := service.OpenPackage(context.Background(), "assembled", path)
		if err != nil {
			firstDownload <- "error: " + err.Error()
			return
		}
		body, _ := io.ReadAll(pkg.Body)
		pkg.Body.Close()
		firstDownload <- string(body)
	}()
	<-opener.firstStarted

	if _, err := service.Index(); err != nil {
		t.Fatal(err)
	}
	close(opener.releaseFirst)
	if got := <-firstDownload; got != "old" {
		t.Fatalf("in-flight package = %q, want old", got)
	}

	pkg, err := service.OpenPackage(context.Background(), "assembled", path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(pkg.Body)
	pkg.Body.Close()
	if string(body) != "new" {
		t.Fatalf("replacement package = %q, want new", body)
	}
}

func TestFirstPartialPublicationContainsOnlySuccessfulContributions(t *testing.T) {
	good := repository.Source{Config: repository.SourceConfig{Name: "good", Kind: "test"}, Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
		return contribution("good", "1.0.0", packageAt("charts/good/good.tgz", "good")), nil
	}), Packages: &recordingOpener{}}
	badOpener := &recordingOpener{}
	bad := repository.Source{Config: repository.SourceConfig{Name: "bad", Kind: "test"}, Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
		return contribution("bad", "1.0.0", packageAt("charts/bad/bad.tgz", "bad")), errors.New("failed")
	}), Packages: badOpener}
	var output bytes.Buffer
	service := repository.New(repository.Sources{good, bad}, repository.WithLogger(logging.New(logging.NewJSON(&output, slog.LevelInfo))))

	index, err := service.Index()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Entries["bad"]; ok {
		t.Fatalf("partial index includes failed contribution: %#v", index.Entries)
	}
	if _, err := service.OpenPackage(context.Background(), "bad", "charts/bad/bad.tgz"); !errors.Is(err, repository.ErrPackageNotFound) {
		t.Fatalf("failed source package error = %v", err)
	}
	if badOpener.Calls() != 0 {
		t.Fatalf("failed source opener calls = %d", badOpener.Calls())
	}

	events := decodeRefreshEvents(t, output.Bytes())
	if len(events) != 1 || events[0]["result"] != "partial" || events[0]["level"] != "WARN" {
		t.Fatalf("events = %#v", events)
	}
	sources := events[0]["sources"].([]any)
	failed := sources[1].(map[string]any)
	if failed["name"] != "bad" || failed["status"] != "failed" || failed["indexed_count"] != float64(0) || failed["skipped_count"] != float64(0) || failed["error"] != "failed" {
		t.Fatalf("failed source = %#v", failed)
	}
}

func TestFreshPublicationIsReusedAndStartupWarmRunsOnceAsynchronously(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	discovery := discoveryFunc(func(context.Context) (repository.Contribution, error) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if current == 1 {
			close(started)
			<-release
		}
		return contribution("widget", "1.0.0", packageAt("charts/assembled/widget.tgz", "ref")), nil
	})
	var output bytes.Buffer
	service := repository.New(repository.Sources{{Config: repository.SourceConfig{Name: "assembled", Kind: "test"}, Discovery: discovery, Packages: &recordingOpener{}}}, repository.WithTTL(time.Minute), repository.WithLogger(logging.New(logging.NewJSON(&output, slog.LevelInfo))))

	returned := make(chan struct{})
	go func() {
		service.Start(nil)
		service.Start(nil)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Start waited for discovery")
	}
	<-started
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := service.Index(); results <- err }()
	}
	close(release)
	// Index returns after the shared build emits its event, so these receives
	// are a completion barrier for both discovery and logging.
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("index did not complete after startup discovery")
		}
	}
	events := decodeRefreshEvents(t, output.Bytes())
	if len(events) != 1 || events[0]["result"] != "published" || events[0]["level"] != "INFO" {
		t.Fatalf("events = %#v", events)
	}
	for _, key := range []string{"trace_id", "refresh_id"} {
		value, _ := events[0][key].(string)
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(value) {
			t.Fatalf("%s = %q", key, value)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", calls)
	}
}

func TestPackageOpeningDoesNotHoldPublicationLock(t *testing.T) {
	openStarted := make(chan struct{})
	releaseOpen := make(chan struct{})
	opener := packageOpenerFunc(func(context.Context, repository.PackageReference) (repository.ChartPackage, error) {
		close(openStarted)
		<-releaseOpen
		return repository.ChartPackage{Body: io.NopCloser(strings.NewReader("bytes")), Filename: "widget.tgz"}, nil
	})
	service := repository.New(repository.Sources{{Config: repository.SourceConfig{Name: "assembled", Kind: "test"}, Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
		return contribution("widget", "1.0.0", packageAt("charts/assembled/widget.tgz", "ref")), nil
	}), Packages: opener}}, repository.WithTTL(0))
	if _, err := service.Index(); err != nil {
		t.Fatal(err)
	}

	openDone := make(chan struct{})
	go func() {
		pkg, _ := service.OpenPackage(context.Background(), "assembled", "charts/assembled/widget.tgz")
		if pkg.Body != nil {
			pkg.Body.Close()
		}
		close(openDone)
	}()
	<-openStarted

	refreshDone := make(chan struct{})
	go func() {
		_, _ = service.Index()
		close(refreshDone)
	}()
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("refresh waited for package opener")
	}
	close(releaseOpen)
	<-openDone
}

type discoveryFunc func(context.Context) (repository.Contribution, error)

func (function discoveryFunc) Discover(ctx context.Context) (repository.Contribution, error) {
	return function(ctx)
}

type packageOpenerFunc func(context.Context, repository.PackageReference) (repository.ChartPackage, error)

func (function packageOpenerFunc) OpenPackage(ctx context.Context, reference repository.PackageReference) (repository.ChartPackage, error) {
	return function(ctx, reference)
}

type recordingOpener struct {
	mu   sync.Mutex
	refs []repository.PackageReference
}

type rotatingOpener struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	firstOnce    sync.Once
}

func (opener *rotatingOpener) OpenPackage(_ context.Context, reference repository.PackageReference) (repository.ChartPackage, error) {
	if reference.Key == "old" {
		opener.firstOnce.Do(func() { close(opener.firstStarted) })
		<-opener.releaseFirst
	}
	return repository.ChartPackage{Body: io.NopCloser(strings.NewReader(reference.Key)), Filename: "widget.tgz"}, nil
}

func (opener *recordingOpener) OpenPackage(_ context.Context, reference repository.PackageReference) (repository.ChartPackage, error) {
	opener.mu.Lock()
	opener.refs = append(opener.refs, reference)
	opener.mu.Unlock()
	return repository.ChartPackage{Body: io.NopCloser(strings.NewReader(reference.Key)), Filename: "widget.tgz"}, nil
}

func (opener *recordingOpener) Calls() int {
	opener.mu.Lock()
	defer opener.mu.Unlock()
	return len(opener.refs)
}

func contribution(name, version string, packages ...repository.DiscoveredPackage) repository.Contribution {
	urls := make([]string, len(packages))
	for i := range packages {
		urls[i] = packages[i].AdvertisedPath
	}
	return repository.Contribution{ChartVersions: []repository.DiscoveredChartVersion{{Version: chart.Version{APIVersion: "v2", Name: name, Version: version, URLs: urls}, Packages: packages}}, IndexedCount: 1}
}

func packageAt(path, key string) repository.DiscoveredPackage {
	return repository.DiscoveredPackage{AdvertisedPath: path, Reference: repository.PackageReference{Source: "assembled", Key: key}}
}

func fixedClock() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }

func TestRefreshEventsReportOutcomesAndSkipCacheHits(t *testing.T) {
	var output bytes.Buffer
	now := fixedClock()
	fail := false
	source := repository.Source{Config: repository.SourceConfig{Name: "source", Kind: "test"}, Discovery: discoveryFunc(func(context.Context) (repository.Contribution, error) {
		if fail {
			return repository.Contribution{IndexedCount: 99, SkippedCount: 99}, errors.New("Get https://user:password@secret.example/chart?token=private: failed")
		}
		return repository.Contribution{IndexedCount: 2, SkippedCount: 3}, nil
	})}
	service := repository.New(repository.Sources{source}, repository.WithClock(func() time.Time { return now }), repository.WithTTL(time.Minute), repository.WithLogger(logging.New(logging.NewJSON(&output, slog.LevelInfo))))
	for _, step := range []struct {
		trace  string
		expire bool
		count  int
	}{{"first", false, 1}, {"cached", false, 1}, {"stale", true, 2}} {
		if step.expire {
			now = now.Add(61 * time.Second)
			fail = true
		}
		if _, err := service.IndexContext(logging.WithTraceID(context.Background(), step.trace)); err != nil {
			t.Fatal(err)
		}
		if events := decodeRefreshEvents(t, output.Bytes()); len(events) != step.count {
			t.Fatalf("%s events = %#v", step.trace, events)
		}
	}
	events := decodeRefreshEvents(t, output.Bytes())
	for i, expected := range []struct{ result, level, trace string }{{"published", "INFO", "first"}, {"stale_retained", "WARN", "stale"}} {
		event := events[i]
		if event["result"] != expected.result || event["level"] != expected.level || event["trace_id"] != expected.trace {
			t.Fatalf("event = %#v", event)
		}
		id, _ := event["refresh_id"].(string)
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
			t.Fatalf("refresh ID = %q", id)
		}
		if _, ok := event["duration_ms"].(float64); !ok {
			t.Fatalf("missing duration: %#v", event)
		}
	}
	if events[0]["refresh_id"] == events[1]["refresh_id"] {
		t.Fatal("refresh ID reused")
	}
	accepted := events[0]["sources"].([]any)[0].(map[string]any)
	failed := events[1]["sources"].([]any)[0].(map[string]any)
	if accepted["indexed_count"] != float64(2) || accepted["skipped_count"] != float64(3) {
		t.Fatalf("accepted source = %#v", accepted)
	}
	if failed["name"] != "source" || failed["status"] != "failed" || failed["indexed_count"] != float64(0) || failed["skipped_count"] != float64(0) || failed["error"] == nil {
		t.Fatalf("failed source = %#v", failed)
	}
	if strings.Contains(output.String(), "private") || strings.Contains(output.String(), "password") {
		t.Fatalf("unsafe log: %s", output.String())
	}
}

func TestRefreshRetainsServiceContextWhenCallerIsCanceled(t *testing.T) {
	var output bytes.Buffer
	service := repository.New(repository.Sources{{Config: repository.SourceConfig{Name: "source", Kind: "test"}, Discovery: discoveryFunc(func(ctx context.Context) (repository.Contribution, error) {
		return repository.Contribution{}, ctx.Err()
	})}}, repository.WithLogger(logging.New(logging.NewJSON(&output, slog.LevelInfo))))
	ctx, cancel := context.WithCancel(logging.WithTraceID(context.Background(), "caller"))
	cancel()
	if _, err := service.IndexContext(ctx); err != nil {
		t.Fatal(err)
	}
	events := decodeRefreshEvents(t, output.Bytes())
	if len(events) != 1 || events[0]["result"] != "published" || events[0]["trace_id"] != "caller" {
		t.Fatalf("events = %#v", events)
	}
}

func decodeRefreshEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err == io.EOF {
			return events
		} else if err != nil {
			t.Fatal(err)
		}
		if event["msg"] != "repository.refresh.completed" {
			t.Fatalf("unexpected event: %#v", event)
		}
		events = append(events, event)
	}
}
