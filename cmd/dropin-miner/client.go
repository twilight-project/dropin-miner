package main

// Helpers every command shares: which config file won and a context that
// ends on Ctrl-C. The tenant key the router meters against is resolved in
// credentials.go.

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/twilight-project/dropin-miner/internal/netdial"
)

// cloneDefaultTransport returns a clone of http.DefaultTransport with only
// DialContext replaced, by netdial.For(dialer). Every field DefaultTransport
// itself tunes — ForceAttemptHTTP2, TLSHandshakeTimeout, IdleConnTimeout,
// MaxIdleConns, ExpectContinueTimeout, its own Proxy — carries over
// unchanged; a client that used to leave Transport nil and rely on
// DefaultTransport implicitly gets the identical shape back, with only the
// dial function named explicitly so a test can intercept it. dialer is
// named by the caller (not built here) so a test can read its own
// Timeout/KeepAlive directly, which a value captured inside this
// function's own closure could not offer.
func cloneDefaultTransport(dialer *net.Dialer) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = netdial.For(dialer)
	return t
}

// describeConfigSource is the one function ruling D-R1 names: the config a
// command reads is resolved in this order — -config, TOKENDROP_CONFIG,
// ./tokendrop.toml, then the installation's own config
// ($TOKENDROP_HOME/tokendrop.toml, else ~/.tokendrop/tokendrop.toml, when
// that file exists) — and only then built-in defaults (the empty return).
// loadConfig, configGatePath and every command that names its config source
// call this one function, so the gate a command takes always keys on
// exactly the file it is about to load.
//
// The first two steps are named regardless of whether the file exists:
// config.Load treats an explicit -config/TOKENDROP_CONFIG as required to
// exist and errors on its own when it does not, and that error should name
// the path the participant gave, not silently fall through to a weaker
// source. The last two steps are soft — picked up only when the file is
// actually there — because neither is something a participant named, and a
// participant who has not written either yet is not making a mistake.
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
	if home := defaultTokendropHome(getenv); home != "" {
		candidate := filepath.Join(home, setupConfigFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// defaultTokendropHome is the installation location this process assumes
// when nothing more specific names one: TOKENDROP_HOME, else ~/.tokendrop.
// Empty when the home directory cannot be determined at all, matching
// configGatePath's own fallback so the two never disagree about where that
// installation is.
func defaultTokendropHome(getenv func(string) string) string {
	if home := getenv("TOKENDROP_HOME"); home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil || userHome == "" {
		return ""
	}
	return filepath.Join(userHome, ".tokendrop")
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
