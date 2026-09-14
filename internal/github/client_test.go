package github

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestClientUsesOptionalTokenForReleaseAPIAndAssetDownloads(t *testing.T) {
	var requests []*http.Request
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request)
		body := `[{"published_at":"2026-08-30T12:00:00Z","assets":[{"id":7,"name":"demo-1.0.0.tgz","url":"https://api.github.test/assets/7"}]}]`
		if strings.HasSuffix(request.URL.Path, "/releases/assets/7") {
			body = "chart bytes"
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	client := NewClient(&http.Client{Transport: transport})

	releases, err := client.ListReleases(context.Background(), "acme", "charts", "secret")
	if err != nil || len(releases) != 1 || releases[0].Assets[0].ID != 7 {
		t.Fatalf("releases = %#v, error = %v", releases, err)
	}
	asset, err := client.DownloadAsset(context.Background(), "acme", "charts", 7, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer asset.Close()
	if data, _ := io.ReadAll(asset); string(data) != "chart bytes" {
		t.Fatalf("asset body = %q", data)
	}
	if len(requests) != 2 || requests[0].Header.Get("Authorization") != "Bearer secret" || requests[1].Header.Get("Accept") != "application/octet-stream" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestClientCanCallPublicRepositoryWithoutToken(t *testing.T) {
	client := NewClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("[]")), Header: make(http.Header)}, nil
	})})
	if _, err := client.ListReleases(context.Background(), "acme", "public", ""); err != nil {
		t.Fatal(err)
	}
}
