package main

// Helpers every command shares: which config file won and a context that
// ends on Ctrl-C. The tenant key the router meters against is resolved in
// credentials.go.

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
