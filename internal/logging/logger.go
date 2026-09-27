// Package logging provides structured logging, correlation, and safe diagnostics.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
)

type Logger interface {
	Debug(context.Context, string, ...any)
	Info(context.Context, string, ...any)
	Warn(context.Context, string, ...any)
	Error(context.Context, string, ...any)
	Request(context.Context, HTTPRequest)
	Response(context.Context, HTTPResponse)
	HTTPClientCompleted(context.Context, string, HTTPClientResponse, ...any)
}

type logger struct{ backend *slog.Logger }

func New(backend *slog.Logger) Logger { return &logger{backend: backend} }

func NewJSON(output io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
}

func (logger *logger) Debug(ctx context.Context, message string, fields ...any) {
	logger.log(ctx, slog.LevelDebug, message, fields)
}

func (logger *logger) Info(ctx context.Context, message string, fields ...any) {
	logger.log(ctx, slog.LevelInfo, message, fields)
}

func (logger *logger) Warn(ctx context.Context, message string, fields ...any) {
	logger.log(ctx, slog.LevelWarn, message, fields)
}

func (logger *logger) Error(ctx context.Context, message string, fields ...any) {
	logger.log(ctx, slog.LevelError, message, fields)
}

func (logger *logger) log(ctx context.Context, level slog.Level, message string, fields []any) {
	if !logger.backend.Enabled(ctx, level) {
		return
	}
	var record slog.Record
	record.Add(contextFields(ctx)...)
	record.Add(fields...)
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, safeAttr(attr))
		return true
	})
	logger.backend.LogAttrs(ctx, level, message, attrs...)
}

func safeAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	switch attr.Value.Kind() {
	case slog.KindAny:
		attr.Value = slog.AnyValue(safeValue(attr.Value.Any()))
	case slog.KindGroup:
		group := attr.Value.Group()
		attrs := make([]slog.Attr, len(group))
		for index, child := range group {
			attrs[index] = safeAttr(child)
		}
		attr.Value = slog.GroupValue(attrs...)
	}
	return attr
}

func safeValue(value any) any {
	switch value := value.(type) {
	case error:
		return RedactError(value)
	case map[string]any:
		fields := make(map[string]any, len(value))
		for key, child := range value {
			fields[key] = safeValue(child)
		}
		return fields
	case []any:
		values := make([]any, len(value))
		for index, child := range value {
			values[index] = safeValue(child)
		}
		return values
	default:
		return value
	}
}

type fieldsKey struct{}
type traceIDKey struct{}

func WithFields(ctx context.Context, fields ...any) context.Context {
	combined := append([]any(nil), contextFields(ctx)...)
	combined = append(combined, fields...)
	return context.WithValue(ctx, fieldsKey{}, combined)
}

func contextFields(ctx context.Context) []any {
	fields, _ := ctx.Value(fieldsKey{}).([]any)
	return fields
}

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(WithFields(ctx, "trace_id", id), traceIDKey{}, id)
}

func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey{}).(string)
	return id
}

func NewTraceID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}
