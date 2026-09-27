package logging

import (
	"context"
	"net/http"
	"time"
)

type HTTPRequest struct {
	Method string
	Path   string
}

type HTTPResponse struct {
	HTTPRequest
	Status       int
	Duration     time.Duration
	BytesWritten int64
	Error        error
}

func (logger *logger) Request(ctx context.Context, request HTTPRequest) {
	logger.Info(ctx, "http.request", "method", request.Method, "path", request.Path)
}

func (logger *logger) Response(ctx context.Context, response HTTPResponse) {
	fields := []any{"method", response.Method, "path", response.Path,
		"duration_ms", response.Duration.Milliseconds(), "bytes_written", response.BytesWritten}
	if response.Status != 0 {
		fields = append(fields, "status", response.Status)
	}
	if response.Error != nil {
		fields = append(fields, "error", response.Error)
	}
	logger.Info(ctx, "http.response", fields...)
}

type HTTPClientResponse struct {
	Method      string
	URL         string
	Status      int
	Duration    time.Duration
	FinalScheme string
	FinalHost   string
	Error       error
}

func (logger *logger) HTTPClientCompleted(ctx context.Context, message string, response HTTPClientResponse, fields ...any) {
	attrs := []any{"method", response.Method, "url", response.URL, "duration_ms", response.Duration.Milliseconds()}
	if response.Status != 0 {
		attrs = append(attrs, "status", response.Status)
	}
	if response.FinalScheme != "" {
		attrs = append(attrs, "final_scheme", response.FinalScheme, "final_host", response.FinalHost)
	}
	if response.Error != nil {
		attrs = append(attrs, "error", response.Error)
	}
	attrs = append(attrs, fields...)
	if response.Error != nil || response.Status >= http.StatusBadRequest {
		logger.Warn(ctx, message, attrs...)
	} else {
		logger.Info(ctx, message, attrs...)
	}
}
