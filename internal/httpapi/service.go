package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"gopkg.in/yaml.v3"
	"helm-github-releases-proxy/internal/catalog"

	"github.com/go-chi/chi/v5"

	"helm-github-releases-proxy/internal/application"
)

// Operations is the application boundary consumed by HTTP handling.
type Operations interface {
	Index() (application.Index, error)
	Status() application.Status
	OpenPackage(context.Context, string, string, string) (application.ChartPackage, error)
	OpenDirectPackage(context.Context, string, string) (application.ChartPackage, error)
}

// Service owns routing and public response representations.
type Service struct {
	configValid bool
	operations  Operations
}

func New(operations Operations, configValid bool) *Service {
	return &Service{configValid: configValid, operations: operations}
}

// Handler returns the service router.
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
	body, err := s.operations.Index()
	if err != nil {
		writePlainError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	data, err := yaml.Marshal(localIndex{APIVersion: "v1", Generated: body.Generated.Format(time.RFC3339Nano), Entries: finalizeEntries(body.Entries)})
	if err != nil {
		writePlainError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeIndex(writer, data)
}

func (s *Service) writeIndex(writer http.ResponseWriter, body []byte) {
	writer.Header().Set("Content-Type", "application/x-yaml")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
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

func (s *Service) status(writer http.ResponseWriter, _ *http.Request) {
	snapshot := s.operations.Status()
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

func (s *Service) chartReleaser(writer http.ResponseWriter, request *http.Request) {
	pkg, err := s.operations.OpenDirectPackage(request.Context(), chi.URLParam(request, "repository"), chi.URLParam(request, "*"))
	writePackage(writer, pkg, err)
}

func (s *Service) chart(writer http.ResponseWriter, request *http.Request) {
	pkg, err := s.operations.OpenPackage(request.Context(), chi.URLParam(request, "repository"), chi.URLParam(request, "asset"), chi.URLParam(request, "filename"))
	writePackage(writer, pkg, err)
}

func writePackage(writer http.ResponseWriter, pkg application.ChartPackage, err error) {
	if errors.Is(err, application.ErrPackageNotFound) {
		writePlainError(writer, http.StatusNotFound, "chart not found")
		return
	}
	if err != nil {
		writePlainError(writer, http.StatusBadGateway, err.Error())
		return
	}
	streamChartPackage(writer, pkg.Body, pkg.Filename)
}

func streamChartPackage(writer http.ResponseWriter, file io.ReadCloser, filename string) {
	defer file.Close()
	writer.Header().Set("Content-Type", "application/gzip")
	writer.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, file)
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

// finalizeEntries translates the pure catalog to the unchanged Helm YAML DTO.
func finalizeEntries(source map[string][]catalog.ChartVersion) map[string][]localIndexEntry {
	result := make(map[string][]localIndexEntry, len(source))
	for name, versions := range source {
		result[name] = make([]localIndexEntry, 0, len(versions))
		for _, v := range versions {
			maintainers := make([]helmMaintainer, len(v.Maintainers))
			for i, m := range v.Maintainers {
				maintainers[i] = helmMaintainer{Name: m.Name, Email: m.Email, URL: m.URL}
			}
			result[name] = append(result[name], localIndexEntry{APIVersion: v.APIVersion, Name: v.Name, Version: v.Version, AppVersion: v.AppVersion, Description: v.Description, Home: v.Home, Icon: v.Icon, Deprecated: v.Deprecated, Keywords: v.Keywords, Maintainers: maintainers, Sources: v.Sources, Annotations: v.Annotations, URLs: v.URLs, Created: v.Created, Digest: v.Digest})
		}
	}
	return result
}
