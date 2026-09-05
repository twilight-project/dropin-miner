package main

// Helpers every command shares: which config file won, a context that
// ends on Ctrl-C, and the tenant key the router meters against.

import (
	"context"
	"os"
	"os/signal"
	"syscall"
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

// apiKey resolves the credential the proxy will forward to the upstream.
// TOKENDROP_API_KEY names the tenant key that makes an observation
// attributable and payable; OPENAI_API_KEY is the fallback every SDK user
// already has exported. Precedence matters: a personal OpenAI key in the
// same shell must not shadow the tenant key and bill the wrong account.
func apiKey(getenv func(string) string) string {
	if k := getenv("TOKENDROP_API_KEY"); k != "" {
		return k
	}
	return getenv("OPENAI_API_KEY")
}
