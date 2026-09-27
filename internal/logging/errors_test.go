package logging_test

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"helm-github-releases-proxy/internal/logging"
)

func TestRedactError(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{`open /var/charts/demo.tgz: permission denied`, `open /var/charts/demo.tgz: permission denied`},
		{`Get "https://user:pass@private.example/path?signature=secret": failed`, `Get "<url-redacted>": failed`},
		{`parse "//private.example/path?signature=secret": failed`, `parse "<url-redacted>": failed`},
		{`parse "https://private.example/path\"secret%zz": invalid escape`, `parse "<url-redacted>": invalid escape`},
		{`read https://private.example/a'b?signature=secret failed`, `read <url-redacted> failed`},
		{`Get "https://api.github.com": failed to parse Location header "/secret%zz?sig=secret": invalid URL escape "%zz"`, `Get "<url-redacted>": failed to parse Location header <url-redacted>`},
	} {
		t.Run(test.input, func(t *testing.T) {
			if got := logging.RedactError(errors.New(test.input)); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
	if got := logging.RedactError(nil); got != "" {
		t.Fatalf("nil error = %q", got)
	}
}

func TestProtectedErrorPreservesPublicMessageAndChain(t *testing.T) {
	cause := errors.New("token secret rejected")
	original := &url.Error{Op: "Get", URL: "https://private.example/path", Err: cause}
	protected := logging.ProtectError(original, "secret")
	wrapped := fmt.Errorf("download: %w", protected)
	var urlError *url.Error
	if protected.Error() != original.Error() || !errors.Is(wrapped, cause) || !errors.As(wrapped, &urlError) || urlError != original {
		t.Fatalf("error changed: %v", wrapped)
	}
	if got := logging.RedactError(wrapped); got != `Get "<url-redacted>": token <token-redacted> rejected` {
		t.Fatalf("diagnostic = %q", got)
	}
}
