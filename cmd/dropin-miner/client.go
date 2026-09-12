package main

// Helpers every command shares: which config file won and a context that
// ends on Ctrl-C. The tenant key the router meters against is resolved in
// credentials.go.

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// describeConfigSource names the file config.Load would have read, for
// error messages and env's trailing comment — the cwd-discovery rule is
// convenient right up until an agent runs somewhere unexpected, and then
// the only cure is saying which file won.
func describeConfigSource(cfgPath string, getenv func(string) string) string {
	if cfgPath != "" {
		return cfgPath
	}
	if p := getenv("TOKENDROP_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("tokendrop.toml"); err == nil {
		return "tokendrop.toml"
	}
	return ""
}

// signalContext cancels on SIGINT/SIGTERM with no deadline — the agent
// decides how long a request may run, not this process. Contrast
// operatorContext, whose timeout exists because enrollment blocks on a
// person.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// searchDeadline is the whole-search budget, derived from the signal
// context so Ctrl-C still cancels promptly.
//
// One absolute instant, created once and shared by everything the search
// does: connect, TLS, headers, body read, and the single trace
// compatibility retry. A fresh timer per attempt would let a search that
// retries take twice as long as the budget an agent was promised, which is
// the same unbounded wait in a costume.
//
// The two cancellations are also distinguishable on purpose. Expiry leaves
// context.DeadlineExceeded; a signal leaves context.Canceled through the
// parent — an agent may retry the first and must not silently retry the
// second.
func searchDeadline(timeout time.Duration) (context.Context, context.CancelFunc) {
	parent, stop := signalContext()
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, func() {
		cancel()
		stop()
	}
}
