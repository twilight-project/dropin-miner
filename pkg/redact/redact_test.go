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

// Batch-1 T3: the shapes redact.String missed before the trace path wired
// it in — sr- (this system's own router key), a dashless sk- key (no
// second word segment between the prefix and the random run), a GitHub
// token, an AWS access key id, a bare JWT, an email address, and a home
// directory path. Each assembled at runtime so no credential-shaped
// literal exists in source (gosec G101).
func TestNewCredentialShapesAreScrubbed(t *testing.T) {
	srKey := "sr-" + strings.Repeat("c", 24) + "canary"
	dashlessSK := "sk" + "-" + strings.Repeat("1", 32)
	ghToken := "ghp_" + strings.Repeat("a", 36)
	awsKey := "AKIA" + strings.Repeat("Q", 16)
	jwt := "eyJ" + strings.Repeat("h", 10) + "." + strings.Repeat("p", 10) + "." + strings.Repeat("s", 10)
	email := "quasarai" + "@" + "protonmail.com"
	homePath := "/Users/" + "realname" + "/.aws/credentials"

	for name, secret := range map[string]string{
		"sr- key": srKey, "dashless sk- key": dashlessSK, "github token": ghToken,
		"aws key": awsKey, "jwt": jwt, "email": email,
	} {
		if out := String("value: " + secret + " end"); strings.Contains(out, secret) {
			t.Errorf("%s not scrubbed: %q", name, out)
		}
	}

	out := String("file: " + homePath)
	if strings.Contains(out, homePath) {
		t.Errorf("home path not scrubbed: %q", out)
	}
	if !strings.Contains(out, ".aws/credentials") {
		t.Errorf("home path scrubbing ate more than the username: %q", out)
	}
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

// PR #1 review: six false positives measured against realistic assistant
// text. Each of these must now survive String() (and, except the bearer
// case, TraceText()) byte-for-byte — this is corpus damage, not a secret.
func TestFalsePositivesMeasuredInReviewSurvive(t *testing.T) {
	cases := map[string]string{
		"git ssh remote":       "git clone git@github.com:acme/repo.git",
		"ssh command":          "ssh deploy@prod.example.com",
		"css accessibility":    `class="sr-only-focusable"`,
		"bcp-47 locale tag":    "locale sr-Latn-RS is Serbian (Latin, Serbia)",
		"generic cache key":    "invalidating cache key sk-cache-entry-42",
		"url path, not a home": "see https://example.com/home/page for docs",
	}
	for name, text := range cases {
		if got := String(text); got != text {
			t.Errorf("%s: String() altered non-secret text:\n  in:  %q\n  out: %q", name, text, got)
		}
		if got := TraceText(text); got != text {
			t.Errorf("%s: TraceText() altered non-secret text:\n  in:  %q\n  out: %q", name, text, got)
		}
	}
}

// The one case that's supposed to differ between the two: ordinary prose
// about tokens is common trajectory data and must survive TraceText, but
// String (the log path, where "bearer" is far more likely to precede an
// actual header value) keeps redacting it.
func TestBearerProseSurvivesTraceTextButNotString(t *testing.T) {
	text := "bearer tokens expire"
	if got := TraceText(text); got != text {
		t.Errorf("TraceText altered ordinary prose about bearer tokens: %q", got)
	}
	if got := String(text); got == text || !strings.Contains(got, placeholder) {
		t.Errorf("String no longer redacts a bearer-shaped value: %q", got)
	}
}

// A real credential riding right next to a false-positive shape must still
// be caught — the narrowing must not have gone too far the other way.
func TestRealSecretsStillCaughtAlongsideFalsePositiveShapes(t *testing.T) {
	secret := "sk-or-v1-" + strings.Repeat("a", 24) + "SECRET"
	text := "class sr-only-focusable, and the key is " + secret
	got := TraceText(text)
	if strings.Contains(got, secret) {
		t.Errorf("real secret survived alongside a false-positive shape: %q", got)
	}
	if !strings.Contains(got, "sr-only-focusable") {
		t.Errorf("the false-positive shape was collateral damage: %q", got)
	}
}

// Windows home paths (C:\Users\<name>\...) — the Unix-only pattern missed
// these entirely on a repo that ships Windows binaries.
func TestWindowsHomePathScrubbed(t *testing.T) {
	path := `C:\Users\` + "realname" + `\.aws\credentials`
	got := TraceText(path)
	if strings.Contains(got, "realname") {
		t.Errorf("Windows username not scrubbed: %q", got)
	}
	if !strings.Contains(got, `C:\Users\`) || !strings.Contains(got, ".aws") {
		t.Errorf("Windows path scrubbing ate more than the username: %q", got)
	}
}

// The email/home-path heuristics must not swallow a genuinely planted
// secret sitting right next to them — the exclusions are about SHAPE
// (immediately followed by ':', preceded by a remote-access verb, part of
// a URL path), not about disabling redaction near those shapes generally.
func TestFalsePositiveExclusionsDoNotShadowRealSecretsNearby(t *testing.T) {
	secret := "sk-or-v1-" + strings.Repeat("b", 24) + "SECRET"
	text := "ssh deploy@prod.example.com and also the key " + secret
	got := TraceText(text)
	if strings.Contains(got, secret) {
		t.Errorf("real secret near an excluded shape was not redacted: %q", got)
	}
}
