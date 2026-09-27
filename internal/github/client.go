package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"helm-github-releases-proxy/internal/logging"
)

const apiBaseURL = "https://api.github.com"

var apiTimeout = 10 * time.Second
var downloadTimeout = 60 * time.Second

type Release struct {
	PublishedAt *time.Time `json:"published_at"`
	Assets      []Asset    `json:"assets"`
}

type Asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
	APIURL string `json:"url"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
	events     logging.Logger
}

func NewClient(httpClient *http.Client, loggers ...*slog.Logger) *Client {
	logger := slog.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return &Client{httpClient: httpClient, baseURL: apiBaseURL, events: logging.New(logger)}
}

type sourceKey struct{}

func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

func (c *Client) ListReleases(ctx context.Context, owner, repository, token string) ([]Release, error) {
	request, err := c.newRequest(ctx, http.MethodGet, "/repos/"+owner+"/"+repository+"/releases", token)
	if err != nil {
		return nil, err
	}
	response, err := c.do(request, apiTimeout, "list_releases")
	if err != nil {
		return nil, fmt.Errorf("list GitHub releases: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return nil, err
	}
	var releases []Release
	if err := json.NewDecoder(response.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode GitHub releases: %w", err)
	}
	return releases, nil
}

func (c *Client) DownloadAsset(ctx context.Context, owner, repository string, assetID int64, token string) (io.ReadCloser, error) {
	path := "/repos/" + owner + "/" + repository + "/releases/assets/" + strconv.FormatInt(assetID, 10)
	request, err := c.newRequest(ctx, http.MethodGet, path, token)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := c.do(request, downloadTimeout, "download_asset")
	if err != nil {
		return nil, fmt.Errorf("download GitHub release asset: %w", err)
	}
	if err := checkResponse(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return response.Body, nil
}

func (c *Client) DownloadRelease(ctx context.Context, owner, repository, tag, filename, token string) (io.ReadCloser, error) {
	request, err := c.newRequest(ctx, http.MethodGet, "/"+owner+"/"+repository+"/releases/download/"+tag+"/"+filename, token)
	if err != nil {
		return nil, err
	}
	request.URL.Scheme = "https"
	request.URL.Host = "github.com"
	request.Header.Set("Accept", "application/octet-stream")
	response, err := c.do(request, downloadTimeout, "download_release")
	if err != nil {
		return nil, fmt.Errorf("download GitHub release package: %w", err)
	}
	if err := checkResponse(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return response.Body, nil
}

// FetchBranchFile reads a file from a repository branch through the contents
// API. The response body is returned directly so chart packages can be
// streamed without buffering them in the proxy.
func (c *Client) FetchBranchFile(ctx context.Context, owner, repository, branch, path, token string) (io.ReadCloser, error) {
	request, err := c.newRequest(ctx, http.MethodGet, "/repos/"+owner+"/"+repository+"/contents/"+path, token)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github.raw+json")
	query := request.URL.Query()
	query.Set("ref", branch)
	request.URL.RawQuery = query.Encode()
	response, err := c.do(request, downloadTimeout, "fetch_branch_file")
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub branch file: %w", err)
	}
	if err := checkResponse(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return response.Body, nil
}

func (c *Client) newRequest(ctx context.Context, method, path, token string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request, nil
}

func (c *Client) client(timeout time.Duration) *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return &http.Client{Timeout: timeout}
}

func (c *Client) do(request *http.Request, timeout time.Duration, operation string) (*http.Response, error) {
	originalURL := request.URL.String()
	client := *c.client(timeout)
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	observed := &finalHostTransport{next: transport}
	client.Transport = observed
	started := time.Now()
	response, err := client.Do(request)
	duration := time.Since(started)
	err = logging.ProtectError(err, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	outcome := logging.HTTPClientResponse{
		Method: request.Method, URL: originalURL, Duration: duration,
		FinalScheme: observed.scheme, FinalHost: observed.host, Error: err}
	if response != nil {
		outcome.Status = response.StatusCode
	}
	c.events.HTTPClientCompleted(request.Context(), "github.request.completed", outcome,
		"source", sourceName(request.Context()), "operation", operation)
	return response, err
}

func sourceName(ctx context.Context) string {
	source, _ := ctx.Value(sourceKey{}).(string)
	return source
}

// Each operation owns its wrapper, so redirects can update these fields without
// sharing state with concurrent requests on the underlying transport.
type finalHostTransport struct {
	next         http.RoundTripper
	scheme, host string
}

func (transport *finalHostTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.scheme, transport.host = request.URL.Scheme, request.URL.Host
	return transport.next.RoundTrip(request)
}

func checkResponse(response *http.Response) error {
	if response.StatusCode < http.StatusBadRequest {
		return nil
	}
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("GitHub resource not found")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return fmt.Errorf("GitHub authentication failed")
	}
	return fmt.Errorf("GitHub API returned status %d", response.StatusCode)
}
