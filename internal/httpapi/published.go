package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"helm-github-releases-proxy/internal/chart"
	"helm-github-releases-proxy/internal/repository"
)

// PublishedOperations is the repository boundary consumed by the assembled app.
type PublishedOperations interface {
	Index() (repository.Index, error)
	Status() repository.Status
	OpenPackage(context.Context, string, string) (repository.ChartPackage, error)
}

type PublishedService struct {
	configValid bool
	operations  PublishedOperations
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

func NewPublished(operations PublishedOperations, configValid bool) *PublishedService {
	return &PublishedService{configValid: configValid, operations: operations}
}

func (service *PublishedService) Handler() http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", service.health)
	router.Get("/readyz", service.ready)
	router.Get("/index.yaml", service.index)
	router.Get("/status", service.status)
	router.Get("/charts/{source}/*", service.packageDownload)
	return router
}

func (service *PublishedService) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, `{"status":"ok"}`)
}

func (service *PublishedService) ready(writer http.ResponseWriter, _ *http.Request) {
	if !service.configValid {
		writeJSON(writer, http.StatusServiceUnavailable, `{"status":"not_ready"}`)
		return
	}
	writeJSON(writer, http.StatusOK, `{"status":"ok"}`)
}

func (service *PublishedService) index(writer http.ResponseWriter, _ *http.Request) {
	index, err := service.operations.Index()
	if err != nil {
		writePlainError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	data, err := yaml.Marshal(publishedIndex{APIVersion: "v1", Generated: index.Generated.Format(time.RFC3339Nano), Entries: publishedEntries(index.Entries)})
	if err != nil {
		writePlainError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/x-yaml")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data)
}

func (service *PublishedService) status(writer http.ResponseWriter, _ *http.Request) {
	snapshot := service.operations.Status()
	response := statusResponse{Status: snapshot.Status, Stale: snapshot.Stale, LastAttempt: snapshot.LastAttempt, LastSuccess: snapshot.LastSuccess, CachedAt: snapshot.CachedAt, CacheExpires: snapshot.CacheExpires, CachePresent: snapshot.CachePresent, LastError: snapshot.LastError, Repositories: make([]repositoryStatus, 0, len(snapshot.Repositories))}
	for _, source := range snapshot.Repositories {
		response.Repositories = append(response.Repositories, repositoryStatus(source))
	}
	data, err := json.Marshal(response)
	if err != nil {
		writePlainError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data)
}

func (service *PublishedService) packageDownload(writer http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.EscapedPath(), "/")
	pkg, err := service.operations.OpenPackage(request.Context(), chi.URLParam(request, "source"), path)
	if errors.Is(err, repository.ErrPackageNotFound) {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	if err != nil {
		writePlainError(writer, http.StatusBadGateway, err.Error())
		return
	}
	defer pkg.Body.Close()
	writer.Header().Set("Content-Type", "application/gzip")
	writer.Header().Set("Content-Disposition", `attachment; filename="`+pkg.Filename+`"`)
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, pkg.Body)
}

type publishedIndex struct {
	APIVersion string                           `yaml:"apiVersion"`
	Generated  string                           `yaml:"generated"`
	Entries    map[string][]publishedIndexEntry `yaml:"entries"`
}

type publishedIndexEntry struct {
	APIVersion  string                `yaml:"apiVersion,omitempty"`
	Name        string                `yaml:"name,omitempty"`
	Version     string                `yaml:"version,omitempty"`
	AppVersion  string                `yaml:"appVersion,omitempty"`
	Description string                `yaml:"description,omitempty"`
	Home        string                `yaml:"home,omitempty"`
	Icon        string                `yaml:"icon,omitempty"`
	Deprecated  *bool                 `yaml:"deprecated,omitempty"`
	Keywords    []string              `yaml:"keywords,omitempty"`
	Maintainers []publishedMaintainer `yaml:"maintainers,omitempty"`
	Sources     []string              `yaml:"sources,omitempty"`
	Annotations map[string]string     `yaml:"annotations,omitempty"`
	URLs        []string              `yaml:"urls,omitempty"`
	Created     string                `yaml:"created,omitempty"`
	Digest      string                `yaml:"digest,omitempty"`
}

type publishedMaintainer struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
	URL   string `yaml:"url,omitempty"`
}

func publishedEntries(source map[string][]chart.Version) map[string][]publishedIndexEntry {
	result := make(map[string][]publishedIndexEntry, len(source))
	for name, versions := range source {
		result[name] = make([]publishedIndexEntry, 0, len(versions))
		for _, version := range versions {
			maintainers := make([]publishedMaintainer, len(version.Maintainers))
			for index, maintainer := range version.Maintainers {
				maintainers[index] = publishedMaintainer{Name: maintainer.Name, Email: maintainer.Email, URL: maintainer.URL}
			}
			result[name] = append(result[name], publishedIndexEntry{APIVersion: version.APIVersion, Name: version.Name, Version: version.Version, AppVersion: version.AppVersion, Description: version.Description, Home: version.Home, Icon: version.Icon, Deprecated: version.Deprecated, Keywords: version.Keywords, Maintainers: maintainers, Sources: version.Sources, Annotations: version.Annotations, URLs: version.URLs, Created: version.Created, Digest: version.Digest})
		}
	}
	return result
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
