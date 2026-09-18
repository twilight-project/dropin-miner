package selfupdate

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Updater prepares an upgrade. Every field has a production default except
// Source, which a command sets to NewHTTPSource(nil).
type Updater struct {
	Source   ReleaseSource
	Verifier ReleaseVerifier // nil: SHA256Verifier
	Runner   CommandRunner   // nil: ExecRunner
	GOOS     string
	GOARCH   string

	// Tests only: the replacement's failure points, and which platform's
	// sequence to run. Nil means the real operations and runtime.GOOS.
	ops     *replaceOps
	windows *bool
	// Tests only: the candidate's per-attempt budget. Zero means
	// CandidateTimeout; nothing outside this package can set it.
	candidateBudget time.Duration
}

func (u Updater) budget() time.Duration {
	if u.candidateBudget > 0 {
		return u.candidateBudget
	}
	return CandidateTimeout
}

// Prepared is a verified candidate staged beside the installed binary, or,
// with NoChange, the news that there is nothing newer.
type Prepared struct {
	From       Version
	To         Version
	NoChange   bool
	Executable string // the resolved installed binary
	Candidate  string // the staged, validated candidate; "" with NoChange
}

// Discard removes the staged candidate before any replacement was
// attempted. It never touches the installed binary.
func (p Prepared) Discard() {
	if p.Candidate != "" {
		_ = os.Remove(p.Candidate)
	}
}

// DiscardAfterInstall is the only cleanup for staging material once Install
// has run: it removes the candidate unless installErr names that path as one
// that survives — a recovery moved the new binary back out to it — so a copy
// the participant is told to find is never deleted. After a successful
// Install the candidate name no longer exists and there is nothing to remove.
func (p Prepared) DiscardAfterInstall(installErr error) {
	if p.Candidate != "" && !errorPreservesPath(installErr, p.Candidate) {
		_ = os.Remove(p.Candidate)
	}
}

// Prepare does everything an upgrade does before replacement, in order:
// refuse a binary that is not a canonical release (before any network),
// select the release, refuse a downgrade (before any download), fetch what
// the verifier requires, verify it, take the executable out of the archive,
// stage it beside the resolved installed binary and run its version command.
// It never renames or replaces the installed binary. A candidate that fails
// validation is removed.
func (u Updater) Prepare(ctx context.Context, executable, currentBuild string, requested *Version) (Prepared, error) {
	current, err := ParseVersion(currentBuild)
	if err != nil || currentBuild != current.String() {
		return Prepared{}, failure(KindNotRelease, fmt.Errorf("this binary reports %q, not a release version; install a release before upgrading", currentBuild))
	}
	if u.Source == nil {
		return Prepared{}, failure(KindUnavailable, fmt.Errorf("no release source"))
	}
	verifier := u.Verifier
	if verifier == nil {
		verifier = SHA256Verifier{}
	}
	resolved, err := ResolveExecutable(executable)
	if err != nil {
		return Prepared{}, failure(KindStaging, err)
	}

	release, err := u.Source.Release(ctx, requested)
	if err != nil {
		return Prepared{}, failure(KindUnavailable, err)
	}
	switch release.Version.Compare(current) {
	case 0:
		return Prepared{From: current, To: current, NoChange: true, Executable: resolved}, nil
	case -1:
		return Prepared{}, failure(KindDowngrade, fmt.Errorf("%s is older than this binary's %s; upgrade never moves backwards (only rollback does)", release.Version, current))
	}

	target, err := ArtifactFor(release.Version, u.GOOS, u.GOARCH)
	if err != nil {
		return Prepared{}, failure(KindUnsupported, err)
	}
	requirements, err := verifier.RequiredAssets(release, target)
	if err != nil {
		return Prepared{}, failure(KindReleaseInvalid, fmt.Errorf("decide what to verify: %w", err))
	}
	assets, err := u.Source.DownloadAssets(ctx, release, requirements)
	if err != nil {
		return Prepared{}, failure(KindUnavailable, err)
	}
	if err := verifier.Verify(ctx, assets, target); err != nil {
		return Prepared{}, failure(KindReleaseInvalid, fmt.Errorf("verify %s: %w", release.Version.Tag(), err))
	}
	binary, err := ArchiveExecutable(assets[target.ArchiveName], target)
	if err != nil {
		return Prepared{}, failure(KindReleaseInvalid, fmt.Errorf("inspect %s: %w", target.ArchiveName, err))
	}
	candidate, err := StageCandidate(resolved, binary)
	if err != nil {
		return Prepared{}, failure(KindStaging, err)
	}
	prepared := Prepared{From: current, To: release.Version, Executable: resolved, Candidate: candidate}
	if err := validateCandidate(ctx, u.Runner, candidate, release.Version, u.budget()); err != nil {
		prepared.Discard()
		return Prepared{}, candidateFailure(KindCandidateInvalid, err)
	}
	return prepared, nil
}
