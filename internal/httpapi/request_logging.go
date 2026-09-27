package httpapi

import (
	"context"
	"net/http"
	"time"

	"helm-github-releases-proxy/internal/logging"
)

// LogRequests observes all routes, including the router's own missing-route response.
func LogRequests(next http.Handler, events logging.Logger) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := logging.WithTraceID(request.Context(), logging.NewTraceID())
		facts := logging.HTTPRequest{Method: request.Method, Path: request.URL.Path}
		started := time.Now()
		events.Request(ctx, facts)
		observed := &observedResponse{ResponseWriter: writer}
		failure := &requestFailure{}
		defer func() {
			events.Response(ctx, logging.HTTPResponse{HTTPRequest: facts, Status: observed.status, Duration: time.Since(started), BytesWritten: observed.bytesWritten, Error: failure.err})
		}()
		next.ServeHTTP(observed, request.WithContext(context.WithValue(ctx, requestFailureKey{}, failure)))
		// Default only on normal return; a panic may leave the status uncommitted.
		if observed.status == 0 {
			observed.status = http.StatusOK
		}
	})
}

type requestFailureKey struct{}
type requestFailure struct{ err error }

// RecordError attaches a handler error to its one completion event.
func RecordError(ctx context.Context, err error) {
	if failure, ok := ctx.Value(requestFailureKey{}).(*requestFailure); ok {
		failure.err = err
	}
}

type observedResponse struct {
	http.ResponseWriter
	status       int
	bytesWritten int64
}

func (writer *observedResponse) WriteHeader(status int) {
	if writer.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		writer.status = status
	}
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *observedResponse) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	n, err := writer.ResponseWriter.Write(body)
	writer.bytesWritten += int64(n)
	return n, err
}

func (writer *observedResponse) Unwrap() http.ResponseWriter { return writer.ResponseWriter }
