package logging

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

type diagnosticError interface{ DiagnosticError() error }

type protectedError struct {
	cause      error
	diagnostic error
}

func (err protectedError) Error() string          { return err.cause.Error() }
func (err protectedError) Unwrap() error          { return err.cause }
func (err protectedError) DiagnosticError() error { return err.diagnostic }

// ProtectError preserves the public error while carrying a redacted diagnostic.
func ProtectError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := RedactError(err)
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "<token-redacted>")
		}
	}
	return protectedError{cause: err, diagnostic: errors.New(message)}
}

var errorURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"<>]+`)
var quotedErrorText = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

// RedactError removes URLs from diagnostics while retaining local paths.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	var diagnostic diagnosticError
	if errors.As(err, &diagnostic) {
		err = diagnostic.DiagnosticError()
	}
	message := err.Error()
	// net/http includes raw relative Location values here, indistinguishable from local paths.
	const malformedLocation = "failed to parse Location header"
	if start := strings.Index(message, malformedLocation); start >= 0 {
		message = message[:start] + malformedLocation + " <url-redacted>"
	}
	message = quotedErrorText.ReplaceAllStringFunc(message, func(quoted string) string {
		value, decodeErr := strconv.Unquote(quoted)
		if decodeErr == nil && (errorURL.MatchString(value) || strings.HasPrefix(value, "//")) {
			return `"<url-redacted>"`
		}
		return quoted
	})
	return errorURL.ReplaceAllString(message, "<url-redacted>")
}
