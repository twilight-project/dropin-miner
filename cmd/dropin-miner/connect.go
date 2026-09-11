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
//
// WP2-adversarial-review hardening (design 009b08d §5.5 "Client hardening
// rules"): every entry point below — cmdConnect and the detached resume
// it spawns — takes connect.lock for its ENTIRE run, exactly as runFlush
// takes flush.lock, so two invocations (a foreground connect and a
// search-spawned resume, or several resumes racing a burst of searches)
// can never interleave. A resume cooldown stamp keeps shouldResume from
// approving a spawn faster than resumes can possibly complete. Every
// mutation to the stored registration re-reads the on-disk record first
// and fails loudly, never silently, on a write error.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/platform"
)

// connectInteractive is the terminal-detection seam for cmdConnect. The
// production value is the real terminal check; tests may force the same
// interactive branch while still driving cmdConnect end to end.
var connectInteractive = isInteractive

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
	// resumeCooldown (WP2-adversarial-review finding 1) is the minimum
	// spacing between resume attempts shouldResume will approve. Without
	// it, a burst of searches in claim-approval-watching range each
	// spawns its own detached resume; connect.lock stops them from
	// interleaving, but a pile of processes that only ever queue on the
	// lock and exit is still waste this stops before it starts.
	resumeCooldown = 5 * time.Second
)

func connectLockPath(stateDir string) string { return filepath.Join(stateDir, "connect.lock") }
func resumeStampPath(stateDir string) string { return filepath.Join(stateDir, "connect_resume.json") }

// resumeStamp records the last time -resume was attempted (successfully
// spawned or not — the point is pacing attempts, not counting successes).
type resumeStamp struct {
	V           int       `json:"v"`
	LastAttempt time.Time `json:"last_attempt"`
}

func readResumeStamp(path string) resumeStamp {
	data, err := os.ReadFile(path) // #nosec G304 -- our own state dir
	if err != nil {
		return resumeStamp{}
	}
	var st resumeStamp
	if json.Unmarshal(data, &st) != nil || st.V != 1 {
		return resumeStamp{}
	}
	return st
}

func writeResumeStamp(path string, st resumeStamp) error {
	st.V = 1
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type registrationPublicationOptions struct {
	force                bool
	replacingExpired     bool
	allowCorruptPreserve bool
	previousAgentID      string
	previousPlatformKey  string
}

func pendingRegistrationFromPlatform(reg *platform.Registration) auth.PendingRegistration {
	return auth.PendingRegistration{
		AgentID:        reg.AgentID,
		Key:            reg.Key,
		ClaimURL:       reg.ClaimURL,
		ClaimCode:      reg.ClaimCode,
		ClaimExpiresAt: reg.ClaimExpiresAt,
		Status:         "unclaimed",
		PollIntervalMS: reg.PollInterval.Milliseconds(),
	}
}

func agentRegistrationFromPending(reg auth.PendingRegistration) auth.AgentRegistration {
	return auth.AgentRegistration{
		AgentID:        reg.AgentID,
		ClaimURL:       reg.ClaimURL,
		ClaimCode:      reg.ClaimCode,
		Status:         reg.Status,
		ClaimExpiresAt: reg.ClaimExpiresAt,
	}
}

func sameAgentRegistration(a auth.AgentRegistration, p auth.PendingRegistration) bool {
	return a.AgentID == p.AgentID && a.ClaimURL == p.ClaimURL && a.ClaimCode == p.ClaimCode &&
		a.Status == p.Status && a.ClaimExpiresAt == p.ClaimExpiresAt
}

func credentialFileState(path string) (exists bool, key string, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return true, "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return true, "", fmt.Errorf("credentials: %s is not a regular file", path)
	}
	creds, err := readCredentials(path)
	if err != nil {
		return true, "", err
	}
	return true, creds.APIKey, nil
}

func preflightFreshRegistration(m config.Miner, force bool) error {
	exists, key, err := credentialFileState(credentialsPath(m))
	if !exists {
		return nil
	}
	if err != nil {
		return fmt.Errorf("credentials.json exists but cannot be trusted; refusing before remote Register: %w", err)
	}
	if !force && looksLikePlatformKey(key) {
		return errors.New("credentials.json already holds a platform key; refusing to mint another agent. Pass -force if you mean to switch this installation to a new registration")
	}
	if !force {
		return errors.New("credentials.json already exists; refusing to overwrite it before remote Register. Pass -force if you mean to replace this installation's platform credential")
	}
	return nil
}

func publishPlatformCredential(path, key string, opts registrationPublicationOptions) error {
	exists, current, err := credentialFileState(path)
	if !exists {
		return writeCredentials(path, credentials{APIKey: key})
	}
	if err != nil {
		return fmt.Errorf("cannot publish platform credential: %w", err)
	}
	if current == key {
		return nil
	}
	if opts.force || (opts.replacingExpired && opts.previousPlatformKey != "" && current == opts.previousPlatformKey) {
		return writeCredentials(path, credentials{APIKey: key})
	}
	return errors.New("cannot publish platform credential: credentials.json contains a different platform key")
}

func publishPendingRegistration(store *auth.Store, m config.Miner, pending auth.PendingRegistration, opts registrationPublicationOptions) (auth.AgentRegistration, error) {
	if err := publishPlatformCredential(credentialsPath(m), pending.Key, opts); err != nil {
		return auth.AgentRegistration{}, err
	}

	reg, ok, err := store.LoadAgentRegistration()
	if err != nil {
		if !errors.Is(err, auth.ErrAgentRegistrationCorrupt) || !opts.allowCorruptPreserve {
			return auth.AgentRegistration{}, fmt.Errorf("cannot publish agent registration: %w", err)
		}
		if err := store.PreserveCorruptAgentRegistration(); err != nil {
			return auth.AgentRegistration{}, err
		}
		ok = false
	}
	if ok && !sameAgentRegistration(reg, pending) {
		replacesExpectedExpired := opts.replacingExpired && reg.Status == "expired" && reg.AgentID == opts.previousAgentID
		if !opts.force && !replacesExpectedExpired {
			return auth.AgentRegistration{}, errors.New("cannot publish agent registration: agent.json contains a different identity")
		}
	}
	if !ok || !sameAgentRegistration(reg, pending) {
		if err := store.SaveAgentRegistration(agentRegistrationFromPending(pending)); err != nil {
			return auth.AgentRegistration{}, err
		}
	}

	verifiedCreds, err := readCredentials(credentialsPath(m))
	if err != nil || verifiedCreds.APIKey != pending.Key {
		return auth.AgentRegistration{}, errors.New("cannot verify published platform credential")
	}
	verifiedReg, ok, err := store.LoadAgentRegistration()
	if err != nil || !ok || !sameAgentRegistration(verifiedReg, pending) {
		return auth.AgentRegistration{}, errors.New("cannot verify published agent registration")
	}
	return verifiedReg, nil
}

func cmdConnect(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("connect", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	name := fs.String("name", "", "a name for this agent (optional)")
	resume := fs.Bool("resume", false, "internal: exactly one poll, act, exit — used by the detached resume search spawns")
	force := fs.Bool("force", false, "overwrite an existing credentials.json even though it already holds a different platform key")
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

	if *resume {
		// Pace attempts before anything else — including before the lock
		// attempt below, so even a resume that finds the lock held (and
		// exits immediately) still counts toward the cooldown shouldResume
		// consults. Best effort: a failed write here costs pacing
		// accuracy, not correctness.
		_ = writeResumeStamp(resumeStampPath(cfg.Mining.StateDir), resumeStamp{LastAttempt: time.Now()})
	}

	// WP2-adversarial-review finding 1: single-flight. This lock is held
	// for cmdConnect's ENTIRE run — register, ask, the whole poll loop,
	// enroll, declare — exactly the shape runFlush already uses for
	// flush.lock. A second connect/connect -resume finds it held and
	// exits quietly rather than racing the first for the same enrollment
	// token, the same dpop.key, or the same declaration.
	lock, held, err := tryLockFile(connectLockPath(cfg.Mining.StateDir))
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	if !held {
		if !*resume {
			fmt.Fprintln(stdout, "connect: another connect is already running; nothing to do")
		}
		return exitOK
	}
	defer func() { _ = unlockFile(lock) }()

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

	// Design item 2.3: "the resume and the flush retry" a pending
	// AS-side revocation `mining disable` could not confirm. Neither
	// the foreground loop nor -resume waits for this — best-effort, and
	// unrelated to the claim/poll work below, which proceeds either way
	// (a search-only registration can never have a pending revoke, but
	// nothing here assumes that). buildMiningClient is only worth the
	// AS round trip when there is actually a marker and an AS to ask —
	// a search-only agent with no [mining] block would otherwise fail
	// this and print nothing useful about it.
	if pending, perr := store.LoadRevokePending(); perr == nil && pending && miningASConfigured(cfg.Mining) {
		if oauthClient, _, berr := buildMiningClient(ctx, cfg.Mining); berr == nil {
			retryPendingRevoke(ctx, store, oauthClient)
		}
	}

	client := platform.New(cfg.Platform.AgentsAPIURL, cfg.Platform.BaseURL)
	br := bufio.NewReader(stdin)

	interval := resumePollInterval
	var reg auth.AgentRegistration
	var existed bool
	var key string

	pending, hasPending, err := store.LoadPendingRegistration()
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	if hasPending {
		reg, err = publishPendingRegistration(store, cfg.Miner, pending, registrationPublicationOptions{
			allowCorruptPreserve: true,
			replacingExpired:     pending.ReplaceExpired,
			previousAgentID:      pending.PreviousAgentID,
			previousPlatformKey:  pending.PreviousKey,
		})
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner: pending registration recovery:", err)
			return exitTransport
		}
		if err := store.ClearPendingRegistration(); err != nil {
			fmt.Fprintln(stderr, "dropin-miner: clear pending registration:", err)
			return exitTransport
		}
		if pending.PollIntervalMS > 0 {
			interval = time.Duration(pending.PollIntervalMS) * time.Millisecond
		}
	} else {
		var loadErr error
		reg, existed, loadErr = store.LoadAgentRegistration()
		if loadErr != nil {
			if !errors.Is(loadErr, auth.ErrAgentRegistrationCorrupt) {
				fmt.Fprintln(stderr, "dropin-miner:", loadErr)
				return exitTransport
			}
			// An unreadable registration may be recoverable only when no
			// platform credential exists beside it. preflightFreshRegistration
			// below enforces that rule before any new Register call.
			fmt.Fprintln(stderr, "dropin-miner: agent registration on file could not be decoded; checking for a credential conflict:", loadErr)
			reg, existed = auth.AgentRegistration{}, false
		}
		if existed && !*resume && reg.Status == "unclaimed" {
			// A foreground connect must discover a live expiry before it
			// prints or follows the old claim target. Detached resume keeps
			// the ordinary one-poll behavior below and never replaces an
			// identity in the background.
			if statusKey, keyErr := platformKey(cfg.Miner); keyErr == nil {
				if st, statusErr := client.Status(ctx, reg.AgentID, statusKey); statusErr == nil && st.Status == "expired" {
					reg.Status = st.Status
					reg.Scopes = st.Scopes
					reg.ClaimExpiresAt = st.ClaimExpiresAt
					if err := store.SaveAgentRegistration(reg); err != nil {
						fmt.Fprintln(stderr, "dropin-miner:", err)
						return exitTransport
					}
				}
			}
		}

		if existed && reg.Status == "expired" {
			if *resume {
				fmt.Fprintln(stdout, "this registration expired before being claimed; run `dropin-miner connect` again for a new one")
				return exitOK
			}

			oldReg := reg
			oldKey, keyErr := platformKey(cfg.Miner)
			replace := false
			if keyErr == nil {
				st, statusErr := client.Status(ctx, oldReg.AgentID, oldKey)
				switch {
				case statusErr == nil && st.Status == "expired":
					replace = true
				case statusErr == nil:
					reg.Status = st.Status
					reg.Scopes = st.Scopes
					reg.ClaimExpiresAt = st.ClaimExpiresAt
					if err := store.SaveAgentRegistration(reg); err != nil {
						fmt.Fprintln(stderr, "dropin-miner:", err)
						return exitTransport
					}
				case errors.Is(statusErr, platform.ErrAgentNotFound):
					fmt.Fprintln(stderr, "dropin-miner: expired registration is no longer known to the platform; refusing automatic replacement")
					return exitTransport
				default:
					fmt.Fprintln(stderr, "dropin-miner: could not safely verify the expired registration before replacement, even with -force:", statusErr)
					return exitTransport
				}
			} else {
				fmt.Fprintln(stderr, "dropin-miner: cannot safely verify the expired registration without its platform credential; refusing replacement even with -force")
				return exitTransport
			}

			if replace {
				outcome, code := decideRegistrationOutcome(stdin, br, stdout, stderr, getenv, cfg, store, connectInteractive(stdin, stdout))
				if code != exitOK {
					return code
				}
				fresh, registerErr := client.Register(ctx, *name, registrationHint(outcome))
				if registerErr != nil {
					fmt.Fprintln(stderr, "dropin-miner: register:", registerErr)
					return exitTransport
				}
				pending = pendingRegistrationFromPlatform(fresh)
				pending.ReplaceExpired = true
				pending.PreviousAgentID = oldReg.AgentID
				pending.PreviousKey = oldKey
				if err := store.SavePendingRegistration(pending); err != nil {
					fmt.Fprintln(stderr, "dropin-miner: persist pending registration:", err)
					return exitTransport
				}
				reg, err = publishPendingRegistration(store, cfg.Miner, pending, registrationPublicationOptions{
					force:                *force,
					replacingExpired:     true,
					allowCorruptPreserve: true,
					previousAgentID:      oldReg.AgentID,
					previousPlatformKey:  oldKey,
				})
				if err != nil {
					fmt.Fprintln(stderr, "dropin-miner: publish replacement registration:", err)
					return exitTransport
				}
				if err := store.ClearPendingRegistration(); err != nil {
					fmt.Fprintln(stderr, "dropin-miner: clear pending registration:", err)
					return exitTransport
				}
				existed = true
				interval = fresh.PollInterval
			}
		}

		if !existed {
			if err := preflightFreshRegistration(cfg.Miner, *force); err != nil {
				fmt.Fprintln(stderr, "dropin-miner:", err)
				return exitTransport
			}
			// Ask before registering: requested_scopes is the hint the claim
			// page pre-ticks its mining grant from, and the terminal answer
			// (or, non-interactively, [mining].enabled) is the only source
			// for it now that there is no -mining flag. participantHasOtherAgent
			// is always false here — this registration doesn't exist yet, so
			// there is nothing to compare against.
			outcome, code := decideRegistrationOutcome(stdin, br, stdout, stderr, getenv, cfg, store, connectInteractive(stdin, stdout))
			if code != exitOK {
				return code
			}
			fresh, err := client.Register(ctx, *name, registrationHint(outcome))
			if err != nil {
				fmt.Fprintln(stderr, "dropin-miner: register:", err)
				return exitTransport
			}
			pending = pendingRegistrationFromPlatform(fresh)
			if err := store.SavePendingRegistration(pending); err != nil {
				fmt.Fprintln(stderr, "dropin-miner: persist pending registration:", err)
				return exitTransport
			}
			reg, err = publishPendingRegistration(store, cfg.Miner, pending, registrationPublicationOptions{
				force:                *force,
				allowCorruptPreserve: true,
			})
			if err != nil {
				fmt.Fprintln(stderr, "dropin-miner: publish registration:", err)
				return exitTransport
			}
			if err := store.ClearPendingRegistration(); err != nil {
				fmt.Fprintln(stderr, "dropin-miner: clear pending registration:", err)
				return exitTransport
			}
			if !*resume {
				interval = fresh.PollInterval
			}
		}
	}

	// Platform calls use the stored agent credential; the search environment
	// override does not apply to this installation's enrollment.
	key, err = platformKey(cfg.Miner)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	// WP2-adversarial-review finding 20: the claim URL/code are a
	// one-time bootstrap artifact, not something to keep echoing back —
	// once reg.Status has ever reached "claimed" they are cleared from
	// the stored record (pollOnce does the clearing) and this print is
	// skipped from then on.
	if !*resume && reg.Status == "unclaimed" {
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
		// WP2-adversarial-review finding 11: the platform's advertised
		// interval is already floor-and-ceiling clamped (pkg/platform),
		// but nothing previously stopped it from parking THIS loop past
		// its own foreground budget — a ceiling on the interval alone
		// does not bound the sleep if the remaining budget is shorter
		// than even the clamped interval. Sleep for whichever is
		// smaller.
		sleep := interval
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		if sleep < 0 {
			sleep = 0
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "dropin-miner: connect: interrupted")
			return exitTransport
		case <-time.After(sleep):
		}
	}
}

// decideRegistrationOutcome is cmdConnect's first-run "ask before
// registering" step (interactive forced by the caller so a test can
// drive it without a real terminal — see isInteractive's own
// constraints). A decision already on the store here means a PRIOR
// connect got as far as asking but never reached Register: most
// plausibly this exact register call failing last time.
//
//   - A prior "no" is read back outright — re-asking an already-declined
//     question on every failed retry is its own kind of unsafe, and
//     there is nothing else to fill in.
//   - A prior "yes" WITH an address already on file is read back too.
//   - A prior "yes" with NO address on file yet (a crash between the two
//     writes on a first run, or an adopted state dir carrying a decision
//     but no address) is NOT read back whole — that would register with
//     the mining hint and silently never ask for an address at all, in
//     violation of design rule 5 ("ask before registering," not "ask
//     once, ever"). finishMiningEnabled asks only the missing half: the
//     address, not the enable question again.
//
// Only an absent decision (UNDECIDED) falls through to askMiningQuestion.
// A present but unreadable decision is DEGRADED and must be repaired
// explicitly; treating it as a first decision would let configuration or a
// new terminal answer overwrite the local runtime authority.
func decideRegistrationOutcome(stdin io.Reader, br *bufio.Reader, stdout, stderr io.Writer, getenv func(string) string, cfg *config.Config, store *auth.Store, interactive bool) (miningEnableOutcome, int) {
	decision := store.ReadMiningDecision()
	switch decision.State {
	case auth.MiningUndecided:
		return askMiningQuestion(stdin, br, stdout, stderr, getenv, cfg, store, interactive, false)
	case auth.MiningDisabled:
		return miningEnableOutcome{}, exitOK
	case auth.MiningDegraded:
		fmt.Fprintln(stderr, "dropin-miner: mining decision is degraded; run `mining enable` or `mining disable` to repair it explicitly")
		return miningEnableOutcome{}, exitTransport
	case auth.MiningEnabled:
		if address, ok, err := store.LoadPayoutAddress(); err == nil && ok {
			return miningEnableOutcome{enabled: true, payoutAddress: address}, exitOK
		}
		return finishMiningEnabled(stdin, br, stdout, stderr, getenv, cfg, store, interactive)
	default:
		fmt.Fprintln(stderr, "dropin-miner: mining decision has an unknown state; run `mining enable` or `mining disable` to repair it explicitly")
		return miningEnableOutcome{}, exitTransport
	}
}

// registrationHint turns the mining question's outcome into register's
// requested_scopes: the claim page's mining pre-tick reads this hint, so
// it must reflect what was actually just decided (or read back — see the
// register-failure retry above), never a value fixed before the question
// was ever asked.
func registrationHint(outcome miningEnableOutcome) []string {
	if outcome.enabled {
		return []string{"mining"}
	}
	return nil
}

// pollOnce checks status once and acts on it: advances the stored
// registration; the moment status shows claimed with the mining scope and
// no enrollment recorded yet, enrolls; and — whether just enrolled this
// call or already enrolled from a past one — declares a payout address on
// file if it is not yet settled. Shared by the foreground loop and the
// detached resume, which calls this exactly once
// (TestDetachedResumePollsOnceAndExits).
//
// "Enrolled" and "declared" are deliberately separate facts
// (WP2-review defect 2): reg.LastEnrollmentSlot != "" used to short-circuit
// this whole function, which meant an address that arrived AFTER
// enrollment (mining enable, run later; or a scripted install that only
// gained mining.payout_address after its first successful poll) was never
// declared, and a declaration that failed transiently right after
// enrollment was never retried. Enrollment and declaration are now two
// independent steps below; either can be a no-op on a given call without
// short-circuiting the other.
//
// done=true means there is nothing further for THIS process to do:
// claimed-and-settled, expired, mining not granted, or a local opt-out.
// done=false means "keep polling" (still unclaimed, or a transient
// error worth retrying in the foreground — a resume does not retry,
// it simply exits and lets the next resume try again).
func pollOnce(ctx context.Context, stdout, stderr io.Writer, client *platform.Client, store *auth.Store, cfg *config.Config, reg *auth.AgentRegistration, key string) (done bool, code int) {
	// WP2-adversarial-review finding 6: re-read the on-disk record before
	// mutating it — reg may be stale relative to whatever the most recent
	// write actually persisted (this function is called repeatedly across
	// a foreground loop's iterations, and separately by every detached
	// resume's own single call; connect.lock keeps those from
	// interleaving, but nothing here should assume that as the ONLY
	// reason the in-memory copy might be stale).
	if fresh, ok, ferr := store.LoadAgentRegistration(); ferr == nil && ok && fresh.AgentID == reg.AgentID {
		*reg = fresh
	}

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
	// WP2-adversarial-review finding 20 stops at status: the claim URL/code
	// are NOT cleared from durable storage, deliberately — mining.go's
	// re-approval message ("mining is not yet granted... re-approve it
	// at: <ClaimURL>", §2.2's last case) reads the exact same stored
	// value later, after the agent is already claimed for search. Clearing
	// it here would silently break that already-shipped flow. status
	// itself already satisfies "never print them again" without any
	// change: printAgentIdentityStatus's "claimed" branch has never
	// printed ClaimURL — only its "unclaimed" branch does.
	if serr := store.SaveAgentRegistration(*reg); serr != nil {
		// finding 6: fail loudly. Continuing as if this succeeded would
		// mean the next call re-derives status from a stale copy again.
		fmt.Fprintln(stderr, "dropin-miner:", serr)
		return true, exitTransport
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

	if reg.LastEnrollmentSlot == "" {
		if !miningActive(store) {
			return true, exitOK // local opt-out or disable (design decision 3)
		}
		if !miningASConfigured(cfg.Mining) {
			fmt.Fprintln(stdout, "the mining scope was granted, but no authorization server is configured — set mining.as_url/chain_id/slot_id, "+
				"then run `dropin-miner connect` or `dropin-miner mining enable` again")
			return true, exitOK
		}

		slot, errText := chooseSlot(st.MiningSlots, cfg.Mining.PlatformSlot)
		if slot == "" {
			if errText == "" {
				errText = "mining scope granted, but the platform offered no slot"
			}
			// WP2-adversarial-review finding 15: persisted so shouldResume
			// stops spawning a resume that can only hit this same wall
			// again, and so status can name it explicitly. Best-effort —
			// the refusal is reported to the caller either way.
			reg.SlotRefusal = errText
			_ = store.SaveAgentRegistration(*reg)
			fmt.Fprintln(stderr, "dropin-miner:", errText)
			return true, exitTransport
		}
		reg.SlotRefusal = ""
		token, err := client.Enroll(ctx, reg.AgentID, key, slot)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner: enroll:", err)
			return true, exitTransport
		}
		oauthClient, _, err := buildMiningClient(ctx, cfg.Mining)
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
		if serr := store.SaveAgentRegistration(*reg); serr != nil {
			// finding 6: the assertion is already redeemed at the AS —
			// the AS now believes this installation is enrolled whether
			// or not this write lands. Silently continuing here would
			// mean the NEXT poll re-attempts enrollment against a token
			// that has already been spent, which fails in a much more
			// confusing way than reporting the real problem now.
			fmt.Fprintln(stderr, "dropin-miner: enrolled, but could not persist the record:", serr)
			return true, exitTransport
		}
		fmt.Fprintln(stdout, "enrolled for mining on", slot)
	}

	// Enrolled now — just above, or on a past call. Declare whatever
	// local address exists and is not yet settled (already active, or
	// held pending an operator); addressSettled avoids the AS round trip
	// once there is nothing left to say. finding 10: the opt-out gates
	// this exactly as it gates enrollment above — an installation that
	// opted out after enrolling once must not keep declaring.
	if miningActive(store) {
		if address, ok, aerr := store.LoadPayoutAddress(); aerr == nil && ok && !addressSettled(store, address) {
			_, miningClient, err := buildMiningClient(ctx, cfg.Mining)
			if err != nil {
				fmt.Fprintln(stderr, "dropin-miner:", err)
				return true, exitOK // enrollment (if any, this call) already succeeded; a declare failure is not fatal to this poll
			}
			declarePayoutIfSafe(ctx, miningClient, store, address, stdout, stderr)
		}
	}
	return true, exitOK
}

// declarePayoutIfSafe is §5.5's read-before-declare rule (design f0ddb69):
// the AS classifies a declaration as first or change on the PARTICIPANT's
// activation history, not the installation, so a second agent declaring
// blind can turn an ordinary "same address again" into a held change an
// operator has to clear. Read the participant's current binding first:
// no active binding, or it already matches local, and declaring is safe
// (a first declaration, or a no-op repeat of the active one — WP4b:
// "a declaration of the same address is not a change"). An active
// binding naming a DIFFERENT address means a real change is in flight
// from some other agent or a prior manual declare; this call declares
// nothing and leaves it for status/an operator, per the design text
// ("declares nothing ... status reports both addresses").
//
// WP2-adversarial-review finding 7: the pre-read above narrows the common
// cases, but it is a read, not a lock — the declare call below can still
// come back HELD for a reason the pre-read could not see (ADDRESS_IN_USE:
// a DIFFERENT participant already holds this exact address, which has
// nothing to do with THIS participant's own active binding) or because
// another installation of this same participant declared in the window
// between the pre-read and this call. The returned *auth.PayoutDeclaration
// — specifically its Effective field, never the mere absence of a
// transport error — is what decides whether this is recorded as settled.
func declarePayoutIfSafe(ctx context.Context, miningClient *auth.MiningClient, store *auth.Store, localAddress string, stdout, stderr io.Writer) {
	// PayoutStanding answers "no active binding" as a normal 200 with
	// Active == nil (payout.go: "Active is the address in force, or
	// nil") — unlike DeclarePayoutAddress's decode path, it has no 404
	// special case, so any error here is a real one, not "nothing
	// declared yet."
	standing, err := miningClient.PayoutStanding(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: payout standing:", err)
		return
	}
	if standing != nil && standing.Active != nil && standing.Active.Address == localAddress {
		// Already in force — a redundant DeclarePayoutAddress call would
		// be harmless (WP4b: "a declaration of the same address is not a
		// change") but is still an AS round trip that changes nothing.
		// Recording it lets a later poll skip even the standing read.
		_ = store.ClearPayoutBindingHeld()
		_ = store.SavePayoutDeclared(localAddress)
		return
	}
	if standing != nil && standing.Active != nil {
		if serr := store.SavePayoutBindingHeld(localAddress, standing.Active.Address, auth.HeldReplacesActive); serr != nil {
			fmt.Fprintln(stderr, "dropin-miner:", serr)
		}
		fmt.Fprintf(stdout, "payout address NOT declared: the AS already has %s active for this participant, "+
			"this installation would declare %s. Run `dropin-miner status` for both addresses; changing the "+
			"binding is an operator-activated change, not something this client can do unattended.\n",
			standing.Active.Address, localAddress)
		return
	}
	doc, derr := miningClient.DeclarePayoutAddress(ctx, localAddress)
	if derr != nil {
		fmt.Fprintln(stderr, "dropin-miner: payout declaration:", derr)
		return
	}
	if !doc.Effective {
		reason := doc.HeldFor
		if reason == "" {
			reason = "unspecified"
		}
		if serr := store.SavePayoutBindingHeld(localAddress, doc.Address, doc.HeldFor); serr != nil {
			fmt.Fprintln(stderr, "dropin-miner:", serr)
		}
		fmt.Fprintf(stdout, "payout address NOT declared: the AS held it (%s). Run `dropin-miner status` for detail; "+
			"changing the binding is an operator-activated change, not something this client can do unattended.\n", reason)
		return
	}
	_ = store.ClearPayoutBindingHeld()
	_ = store.SavePayoutDeclared(localAddress)
	fmt.Fprintln(stdout, "payout address declared:", localAddress)
}

// addressSettled reports whether address needs no further declare attempt
// this poll.
//
// WP2-adversarial-review finding 8: only a CONFIRMED, matching declaration
// (SavePayoutDeclared) is terminal. A hold (LoadPayoutBindingHeld) used to
// be treated as terminal too, which meant an operator activating the
// change later was never noticed — status kept showing a stale HELD
// forever, because nothing ever asked the AS again. A hold is re-checked
// on every foreground command (status, connect, mining enable each call
// this indirectly by re-attempting the declare) and by the resume, paced
// by resumeCooldown.
func addressSettled(store *auth.Store, address string) bool {
	declared, ok, err := store.LoadPayoutDeclared()
	return err == nil && ok && declared == address
}

// chooseSlot picks which platform-advertised slot to enroll into.
//
// WP2-review ruling on judgment call 1: one slot offered, take it. More
// than one, refuse and require mining.platform_slot naming which one —
// automatic matching against the AS's own audience is a question for the
// AS discovery document, not something this client guesses at. errText is
// empty unless a genuine refusal happened (more than one slot, and either
// no platform_slot configured or one that names none of them), in which
// case it names what was offered so the message the caller prints is
// actionable.
func chooseSlot(slots []string, platformSlot string) (slot, errText string) {
	switch len(slots) {
	case 0:
		return "", ""
	case 1:
		return slots[0], ""
	}
	if platformSlot != "" {
		for _, s := range slots {
			if s == platformSlot {
				return s, ""
			}
		}
		return "", fmt.Sprintf("mining.platform_slot %q does not match any slot the platform offered (%s)",
			platformSlot, strings.Join(slots, ", "))
	}
	return "", fmt.Sprintf("the platform offered more than one mining slot (%s); set mining.platform_slot to name which one",
		strings.Join(slots, ", "))
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
// detached connect -resume for. A pure disk read, no network — search.go
// calls this after every served search, independent of [mining]/[miner]
// being configured at all (a search-only unclaimed participant has
// neither).
//
// WP2-review defect 3: the spawn must happen only while a resume can
// actually do something. "Claimed, mining scope, not yet enrolled" used to
// be treated as permanently actionable, but two sub-states of it never
// resolve on their own — a local opt-out and a granted scope with no
// [mining] block configured at all — and a resume in either state just
// polls the platform and exits, forever, on every single search. Both
// return false. Symmetrically, an ALREADY-enrolled registration is still
// actionable if it has an on-file payout address that addressSettled says
// is not yet settled — an enrolled-but-undeclared installation is not
// done (WP2-adversarial-review finding 8: nor is one whose declaration is
// HELD — that gets re-checked here too, paced by resumeCooldown, not the
// 5s connect.lock/resume-only pacing on its own).
//
// WP2-adversarial-review finding 1: paced by resumeCooldown so a burst of
// searches cannot approve more spawns than connect.lock could ever let
// run concurrently anyway. Finding 10: mirrors miningActive exactly,
// the same check pollOnce uses before both enrollment and declaration.
// Finding 15: a persisted multi-slot refusal is treated the same as
// "unconfigured" — permanent until the config actually changes. Finding
// 17: a registration record that fails to load (corrupt, empty) resumes
// nothing; recovery is a manual connect, not a silent background retry.
//
// A revoke_pending marker (mining disable, when the AS-side revocation
// could not complete) is worth a resume on its own, independent of
// reg.Status entirely — retrying it is the resume's job, alongside
// pollOnce's, per design item 2.3 ("the resume and the flush retry
// while the marker exists"); there would otherwise be no other reason
// left to spawn one once mining is stopped locally.
//
// Deliberately NOT auth.OpenStore(stateDir) first: that creates the
// state directory (MkdirAll) if it does not exist, which would give
// every plain search a filesystem side effect on a machine that has
// never run connect and never will. The os.Stat below is read-only and
// costs nothing when there is nothing to resume — the overwhelmingly
// common case.
func shouldResume(cfg *config.Config) bool {
	stateDir := cfg.Mining.StateDir
	if stateDir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(stateDir, "agent.json")); err != nil {
		return false
	}
	if st := readResumeStamp(resumeStampPath(stateDir)); time.Since(st.LastAttempt) < resumeCooldown {
		return false
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		return false
	}
	if pending, perr := store.LoadRevokePending(); perr == nil && pending {
		return true
	}
	reg, ok, err := store.LoadAgentRegistration()
	if err != nil || !ok {
		return false // finding 17: corrupt or absent — nothing to resume
	}
	switch reg.Status {
	case "unclaimed":
		return true
	case "claimed":
		if !hasScope(reg.Scopes, "mining") {
			return false // search-only claim: settled, nothing left to resume
		}
		if reg.LastEnrollmentSlot == "" {
			if !miningActive(store) || reg.SlotRefusal != "" {
				return false // opted out/disabled, a multi-slot refusal on file, or nothing configured — either way, permanent until reconfigured
			}
			if !miningASConfigured(cfg.Mining) {
				return false
			}
			return true
		}
		if !miningActive(store) {
			return false
		}
		address, hasAddr, aerr := store.LoadPayoutAddress()
		if aerr != nil || !hasAddr {
			return false // enrolled, nothing to declare
		}
		return !addressSettled(store, address)
	default:
		return false // expired, or unrecognized: nothing a resume can do
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
