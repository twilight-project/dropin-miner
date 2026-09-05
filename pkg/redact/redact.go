// Package redact is the single place a log handler is constructed and the
// choke point every log line passes through. No credential, header value, or
// URL userinfo may reach a log sink, whatever path it takes to get there:
// slog attributes, error strings, or the pre-formatted lines net/http writes
// to its *log.Logger fields. No package outside this one and cmd/ may build a
// slog.Handler or a *log.Logger.
package redact

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const placeholder = "[REDACTED]"

// Attribute keys whose values are never rendered, matched case-insensitively.
var keyDenylist = map[string]struct{}{
	"authorization":       {},
	"api_key":             {},
	"token":               {},
	"cookie":              {},
	"set-cookie":          {},
	"proxy-authorization": {},
}

var (
	// Provider key material (sk-or-v1-…, sk-ant-…). The trailing run is
	// swallowed so no fragment of the key survives.
	skPattern = regexp.MustCompile(`sk-[a-z]+-\S*`)
	// Bearer values in free text (error strings, net/http log lines).
	bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	// scheme://user:pass@ userinfo embedded in a URL.
	userinfoPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)
)

// String scrubs credential-shaped substrings from free text.
func String(s string) string {
	s = skPattern.ReplaceAllString(s, placeholder)
	s = bearerPattern.ReplaceAllString(s, "Bearer "+placeholder)
	s = userinfoPattern.ReplaceAllString(s, "${1}"+placeholder+"@")
	return s
}

// Error returns an error whose text has been scrubbed. The original error is
// deliberately not wrapped: keeping it reachable via Unwrap would keep the
// unscrubbed text reachable too.
func Error(err error) error {
	if err == nil {
		return nil
	}
	return redactedError(String(err.Error()))
}

type redactedError string

func (e redactedError) Error() string { return string(e) }

// NewLogger builds the process logger: JSON to w, filtered at level, every
// record passing through the scrubbing handler.
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(&handler{inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
}

// NewStdLogger adapts the structured logger for net/http's two *log.Logger
// fields (http.Server.ErrorLog, httputil.ReverseProxy.ErrorLog). Both write
// pre-formatted lines that would otherwise bypass redaction entirely.
func NewStdLogger(logger *slog.Logger, level slog.Level) *log.Logger {
	return log.New(&slogWriter{logger: logger, level: level}, "", 0)
}

type slogWriter struct {
	logger *slog.Logger
	level  slog.Level
}

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSuffix(string(p), "\n")
	w.logger.Log(context.Background(), w.level, String(msg))
	return len(p), nil
}

type handler struct {
	inner slog.Handler
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = scrubAttr(a)
	}
	return &handler{inner: h.inner.WithAttrs(scrubbed)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name)}
}

func scrubAttr(a slog.Attr) slog.Attr {
	if _, deny := keyDenylist[strings.ToLower(a.Key)]; deny {
		return slog.String(a.Key, placeholder)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		group := v.Group()
		members := make([]any, 0, len(group))
		for _, g := range group {
			members = append(members, scrubAttr(g))
		}
		return slog.Group(a.Key, members...)
	case slog.KindString:
		return slog.String(a.Key, String(v.String()))
	case slog.KindAny:
		return slog.String(a.Key, renderAny(v.Any()))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

// renderAny refuses header maps and userinfo wholesale and scrubs everything
// else through its string form.
func renderAny(v any) string {
	switch t := v.(type) {
	case http.Header, *http.Header, *url.Userinfo:
		return placeholder
	case error:
		return String(t.Error())
	case fmt.Stringer:
		return String(t.String())
	default:
		return String(fmt.Sprint(v))
	}
}
