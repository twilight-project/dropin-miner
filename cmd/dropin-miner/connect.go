package main

// connect: the agent-onboarding client (agent onboarding design,
// search-platform-agent-onboarding-design.md §5.5). It registers with
// the search platform, prints the claim URL and code, polls (bounded)
// until claimed, and — the moment status shows the mining scope granted
// — enrolls unattended, reusing RedeemEnrollmentAssertion exactly as
// `enroll -assertion` does today. search works from the moment this
// stores the key, before any of that.
//
// Resume is one poll, not a loop (§5.5): after a served search, search.go
// checks the local registration file with no network and spawns a
// detached `connect -resume`, the same spawnFlush pattern. The resume
// performs exactly one poll, acts, exits — anything else is a daemon by
// another name.
//
// Structural invariant 11: this file and mining.go carry no os/exec
// import. Nothing about registering, polling or enrolling legitimately
// launches a process; the only spawn in this codebase is search.go's
// existing detached child, and TestConnectAndMiningNeverImportOSExec
// (boundary_test.go) enforces it.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/platform"
)

// connectPollBudget and resumePollInterval are vars, not consts, solely
// so a test can shrink them (save, override, t.Cleanup restore) instead
// of waiting out a real 3-minute bound — the production default is what
// matters, and nothing in cmd/dropin-miner ever overwrites it outside a
// test.
var (
	// connectPollBudget bounds the foreground poll: a human is watching
	// for at most this long, then connect stops and finishes on the next
	// invocation — a detached resume, or a later foreground run.
	connectPollBudget = 3 * time.Minute
	// resumePollInterval paces a resumed poll (foreground repeats, and
	// every detached resume). The server only advertises an interval on
	// Register (§5.1); a resume does not re-register, so there is
	// nothing fresher to honor — a fixed, modest default instead of
	// persisting the original registration's value.
	resumePollInterval = 5 * time.Second
)

func cmdConnect(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("connect", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	name := fs.String("name", "", "a name for this agent (optional)")
	miningHint := fs.Bool("mining", false, "hint the claim page to pre-tick the mining grant (requested_scopes); grants nothing by itself")
	resume := fs.Bool("resume", false, "internal: exactly one poll, act, exit — used by the detached resume search spawns")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, cfgSource, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: config (%s): %v\n", orDefaults(cfgSource), err)
		return exitTransport
	}
	store, err := auth.OpenStore(cfg.Mining.StateDir)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	// ctx bounds SIGINT/SIGTERM cancellation and, for -resume, its one
	// round trip's worth of patience. It deliberately does NOT carry the
	// foreground loop's connectPollBudget: that bound is a plain
	// wall-clock deadline checked between iterations below, each of
	// which gets its own short-lived derived context. Tying ctx itself
	// to connectPollBudget raced the graceful "not claimed yet" exit
	// against ctx.Done() firing mid-select, and ctx.Done() usually won —
	// every foreground call surfaced as "connect: interrupted" instead
	// of the friendly timeout message, because the deadline and the
	// context's own timeout expired at effectively the same instant.
	roundTrip := 30 * time.Second
	outerBound := roundTrip
	if !*resume {
		outerBound = 24 * time.Hour // signal-cancellation only, in practice
	}
	ctx, cancel := operatorContext(outerBound)
	defer cancel()

	client := platform.New(cfg.Platform.BaseURL)
	br := bufio.NewReader(stdin)

	reg, existed, err := store.LoadAgentRegistration()
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	interval := resumePollInterval
	if !existed {
		var requestedScopes []string
		if *miningHint {
			requestedScopes = []string{"mining"}
		}
		fresh, err := client.Register(ctx, *name, requestedScopes)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner: register:", err)
			return exitTransport
		}
		if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: fresh.Key}); err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return exitTransport
		}
		reg = auth.AgentRegistration{
			AgentID:        fresh.AgentID,
			ClaimURL:       fresh.ClaimURL,
			ClaimCode:      fresh.ClaimCode,
			Status:         "unclaimed",
			ClaimExpiresAt: fresh.ClaimExpiresAt,
		}
		if err := store.SaveAgentRegistration(reg); err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return exitTransport
		}
		if !*resume {
			interval = fresh.PollInterval
		}
	}

	key, _, err := resolveAPIKey(getenv, cfg.Miner)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	if key == "" {
		fmt.Fprintln(stderr, "dropin-miner: no api key resolved after registration; TOKENDROP_API_KEY or OPENAI_API_KEY may be shadowing the freshly stored one")
		return exitTransport
	}

	// The one-time mining question, first run only: a second run finds
	// existed=true above and skips straight to polling.
	if !existed {
		if _, code := askMiningQuestion(stdin, br, stdout, stderr, getenv, cfg, store, isInteractive(stdin, stdout)); code != exitOK {
			return code
		}
	}

	if !*resume {
		fmt.Fprintln(stdout, "claim this agent:")
		fmt.Fprintln(stdout, "  "+reg.ClaimURL)
		if reg.ClaimCode != "" {
			fmt.Fprintln(stdout, "code:", reg.ClaimCode)
		}
	}

	if *resume {
		callCtx, callCancel := context.WithTimeout(ctx, roundTrip)
		_, code := pollOnce(callCtx, stdout, stderr, client, store, cfg, &reg, key)
		callCancel()
		return code
	}

	deadline := time.Now().Add(connectPollBudget)
	for {
		callCtx, callCancel := context.WithTimeout(ctx, roundTrip)
		done, code := pollOnce(callCtx, stdout, stderr, client, store, cfg, &reg, key)
		callCancel()
		if done {
			return code
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(stdout, "\nnot claimed yet. Approve it at the URL above, then run `dropin-miner connect` again")
			fmt.Fprintln(stdout, "(or just keep using `search` — it resumes this automatically once network is available).")
			return exitOK
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "dropin-miner: connect: interrupted")
			return exitTransport
		case <-time.After(interval):
		}
	}
}

// pollOnce checks status once and acts on it: advances the stored
// registration, and — the moment status shows claimed with the mining
// scope and no enrollment recorded yet — enrolls and, if a payout
// address is already on file (askMiningQuestion, or a scripted
// mining.payout_address), declares it unattended. Shared by the
// foreground loop and the detached resume, which calls this exactly
// once (TestDetachedResumePollsOnceAndExits).
//
// done=true means there is nothing further for THIS process to do:
// claimed-and-settled, expired, mining not granted, or a local opt-out.
// done=false means "keep polling" (still unclaimed, or a transient
// error worth retrying in the foreground — a resume does not retry,
// it simply exits and lets the next resume try again).
func pollOnce(ctx context.Context, stdout, stderr io.Writer, client *platform.Client, store *auth.Store, cfg *config.Config, reg *auth.AgentRegistration, key string) (done bool, code int) {
	st, err := client.Status(ctx, reg.AgentID, key)
	if errors.Is(err, platform.ErrAgentNotFound) {
		fmt.Fprintln(stderr, "dropin-miner: this agent is no longer known to the platform (revoked, or lost server-side)")
		return true, exitTransport
	}
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: status:", err)
		return false, exitTransport
	}

	reg.Status = st.Status
	reg.Scopes = st.Scopes
	reg.ClaimExpiresAt = st.ClaimExpiresAt
	if serr := store.SaveAgentRegistration(*reg); serr != nil {
		fmt.Fprintln(stderr, "dropin-miner:", serr)
	}

	switch st.Status {
	case "expired":
		fmt.Fprintln(stdout, "this registration expired before being claimed; run `dropin-miner connect` again for a new one")
		return true, exitOK
	case "unclaimed":
		return false, exitOK
	}

	// claimed.
	if !st.HasScope("mining") {
		return true, exitOK // search-only claim: nothing further for connect to do
	}
	if reg.LastEnrollmentSlot != "" {
		return true, exitOK // already enrolled
	}
	if cfg.MiningEnabledExplicit && !cfg.Mining.Enabled {
		return true, exitOK // local opt-out (design decision 3)
	}
	if !cfg.Mining.Enabled || cfg.Mining.ASBaseURL == "" {
		fmt.Fprintln(stdout, "the mining scope was granted, but this installation's [mining] block "+
			"names no authorization server yet — set mining.as_url/chain_id/slot_id and mining.enabled = true, "+
			"then run `dropin-miner connect` or `dropin-miner mining enable` again")
		return true, exitOK
	}

	slot := chooseSlot(st.MiningSlots)
	if slot == "" {
		fmt.Fprintln(stderr, "dropin-miner: mining scope granted, but the platform offered no slot")
		return true, exitTransport
	}
	token, err := client.Enroll(ctx, reg.AgentID, key, slot)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: enroll:", err)
		return true, exitTransport
	}
	oauthClient, miningClient, err := buildMiningClient(ctx, cfg.Mining)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return true, exitTransport
	}
	if _, err := oauthClient.RedeemEnrollmentAssertion(ctx, token); err != nil {
		fmt.Fprintln(stderr, "dropin-miner: redeem enrollment:", err)
		return true, exitTransport
	}
	reg.LastEnrollmentSlot = slot
	reg.LastEnrollmentAt = time.Now().UTC().Format(time.RFC3339)
	_ = store.SaveAgentRegistration(*reg)
	fmt.Fprintln(stdout, "enrolled for mining on", slot)

	if address, ok, aerr := store.LoadPayoutAddress(); aerr == nil && ok {
		if _, derr := miningClient.DeclarePayoutAddress(ctx, address); derr != nil {
			fmt.Fprintln(stderr, "dropin-miner: payout declaration:", derr)
		} else {
			fmt.Fprintln(stdout, "payout address declared:", address)
		}
	}
	return true, exitOK
}

// chooseSlot picks which platform-advertised slot to enroll into.
//
// JUDGMENT CALL, flagged for confirmation: the design's §5.2 examples
// always show exactly one slot ("slots": ["twilight-slot-3"]), and
// nothing in §5 states how a platform slot NAME maps to this client's
// configured mining.slot_id (a number, the AS's own identifier) when
// more than one is offered. Until that mapping exists, the first
// offered slot is used — correct for the one-slot case the design
// documents, silently arbitrary if a platform ever offers more than one.
func chooseSlot(slots []string) string {
	if len(slots) == 0 {
		return ""
	}
	return slots[0]
}

// buildMiningClient constructs the AS mining client from an
// already-resolved config.Mining, mirroring enroll.go's miningClients
// construction exactly (same steps, same error wrapping) without its
// own config.Load call — miningClients hardcodes os.Getenv, and connect
// already has a resolved *config.Config built with the injected getenv.
func buildMiningClient(ctx context.Context, m config.Mining) (*auth.OAuthClient, *auth.MiningClient, error) {
	store, err := auth.OpenStore(m.StateDir)
	if err != nil {
		return nil, nil, fmt.Errorf("key store: %w", err)
	}
	disc, err := auth.NewDiscoverer(auth.DiscoveryConfig{
		BaseURL: m.ASBaseURL, ChainID: m.ChainID, SlotID: m.SlotID, TTL: m.MetadataTTL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("discovery: %w", err)
	}
	oauthClient, err := auth.NewOAuthClient(ctx, disc, store)
	if err != nil {
		return nil, nil, fmt.Errorf("authorization server: %w", err)
	}
	return oauthClient, auth.NewMiningClient(disc, oauthClient, store), nil
}

// ── detached resume ─────────────────────────────────────────────────────

// shouldResume reports whether a stored registration is worth spawning a
// detached connect -resume for: unclaimed (still waiting on the human),
// or claimed with the mining scope but not yet enrolled. A pure disk
// read, no network — search.go calls this after every served search,
// independent of [mining]/[miner] being configured at all (a
// search-only unclaimed participant has neither).
//
// Deliberately NOT auth.OpenStore(stateDir) first: that creates the
// state directory (MkdirAll) if it does not exist, which would give
// every plain search a filesystem side effect on a machine that has
// never run connect and never will. The os.Stat below is read-only and
// costs nothing when there is nothing to resume — the overwhelmingly
// common case.
func shouldResume(stateDir string) bool {
	if stateDir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(stateDir, "agent.json")); err != nil {
		return false
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		return false
	}
	reg, ok, err := store.LoadAgentRegistration()
	if err != nil || !ok {
		return false
	}
	switch reg.Status {
	case "unclaimed":
		return true
	case "claimed":
		return reg.LastEnrollmentSlot == "" && hasScope(reg.Scopes, "mining")
	default:
		return false
	}
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// startConnectResume launches `connect -resume` as a detached child,
// the exact shape of startFlush (miner.go): best effort, output goes
// nowhere, the next search or resume tries again if this one could not
// even start.
func startConnectResume(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"connect", "-resume"}
	if cfgPath != "" {
		args = append(args, "-config", cfgPath)
	}
	return spawnDetached(exe, args)
}
