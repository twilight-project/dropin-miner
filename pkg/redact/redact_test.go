package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Synthetic values assembled at runtime so no credential-shaped literal
// exists in the source (gosec G101, and the spirit of testdata/README.md).
var (
	fakeKey    = "sk-or-v1-" + strings.Repeat("0", 4) + "synthetic"
	fakeBearer = strings.Join([]string{"Bearer", "0000synthetic0000"}, " ")
)

func logAndCapture(t *testing.T, logFn func(l *slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	logFn(NewLogger(&buf, slog.LevelDebug))
	return buf.String()
}

func mustNotContain(t *testing.T, out string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("log output contains %q:\n%s", s, out)
		}
	}
}

func TestDenylistedKeys(t *testing.T) {
	for _, key := range []string{
		"authorization", "Authorization", "AUTHORIZATION",
		"api_key", "token", "cookie", "set-cookie", "proxy-authorization",
	} {
		out := logAndCapture(t, func(l *slog.Logger) {
			l.Info("m", key, "secret-value-123")
		})
		mustNotContain(t, out, "secret-value-123")
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("key %q: expected placeholder in output:\n%s", key, out)
		}
	}
}

func TestCredentialPatternsInValues(t *testing.T) {
	out := logAndCapture(t, func(l *slog.Logger) {
		l.Info("upstream said "+fakeKey,
			"detail", "request with "+fakeKey+" failed",
			"hdr", fakeBearer,
		)
	})
	mustNotContain(t, out, fakeKey, "0000synthetic0000")
}

func TestURLUserinfoScrubbed(t *testing.T) {
	out := logAndCapture(t, func(l *slog.Logger) {
		l.Info("dial", "url", "https://user:pass@openrouter.ai/api")
	})
	mustNotContain(t, out, "user:pass")
	if !strings.Contains(out, "openrouter.ai") {
		t.Errorf("host should survive redaction:\n%s", out)
	}
}

func TestHeaderAndUserinfoValuesRefused(t *testing.T) {
	h := http.Header{"Authorization": []string{fakeBearer}, "X-Ok": []string{"fine"}}
	out := logAndCapture(t, func(l *slog.Logger) {
		l.Info("m", "headers", h, "user", url.UserPassword("u", "p"))
	})
	// The entire header map is refused, including innocuous values.
	mustNotContain(t, out, "0000synthetic0000", "fine", `"p"`)
}

func TestGroupsAreWalked(t *testing.T) {
	out := logAndCapture(t, func(l *slog.Logger) {
		l.Info("m", slog.Group("req", slog.String("authorization", "sec"), slog.String("note", fakeKey)))
	})
	mustNotContain(t, out, `"sec"`, fakeKey)
}

func TestWithAttrsScrubbed(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, slog.LevelDebug).With("api_key", fakeKey)
	l.Info("m")
	mustNotContain(t, buf.String(), fakeKey)
}

func TestErrorRedaction(t *testing.T) {
	err := errors.New("Get \"https://user:pass@host/x\": auth " + fakeKey)
	red := Error(err)
	if strings.Contains(red.Error(), fakeKey) || strings.Contains(red.Error(), "user:pass") {
		t.Errorf("Error() leaked: %s", red)
	}
	if Error(nil) != nil {
		t.Error("Error(nil) must be nil")
	}
	// The original text must not be reachable through the chain.
	if errors.Unwrap(red) != nil {
		t.Error("redacted error must not unwrap to the original")
	}
}

func TestStdLoggerAdapter(t *testing.T) {
	var buf bytes.Buffer
	std := NewStdLogger(NewLogger(&buf, slog.LevelDebug), slog.LevelError)
	std.Printf("http: proxy error: dial https://u:p@x.test with %s", fakeKey)
	mustNotContain(t, buf.String(), fakeKey, "u:p")
	if !strings.Contains(buf.String(), "proxy error") {
		t.Errorf("message body lost:\n%s", buf.String())
	}
}

func TestErrorAttrScrubbed(t *testing.T) {
	out := logAndCapture(t, func(l *slog.Logger) {
		l.Error("fail", "err", errors.New("boom "+fakeKey))
	})
	mustNotContain(t, out, fakeKey)
	if !strings.Contains(out, "boom") {
		t.Errorf("error text lost:\n%s", out)
	}
}
