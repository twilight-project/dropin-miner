// Package selfupdate finds, verifies and stages a canonical DropinMiner
// release for a native self-upgrade.
//
// Prepare is the half of an upgrade that happens before anything is replaced:
// select a stable release from the one compiled-in GitHub repository, fetch
// the assets a verifier declares under their bounds, verify them, take the
// single executable out of the archive, write it beside the installed binary
// and run only its version command. Replace is the other half: install a
// validated candidate over the canonical binary, validate the canonical path
// itself, and only then keep the displaced binary as the one-level
// .previous. Rollback restores that .previous through the same transaction,
// with no network.
//
// Boundaries, recorded in AGENTS.md's import-boundaries section: this package
// imports nothing from cmd/, holds and sends no participant credential, and
// its one HTTP client follows redirects only over HTTPS, at most five times,
// and only to the four GitHub hosts a release download uses. Ownership of the
// running binary (npm or native) is decided by the command, through the one
// classifier in cmd/dropin-miner, before this package is used.
package selfupdate

import (
	"errors"
	"time"
)

// The frozen bounds. A legitimate release approaching half of one is a
// question for review, not a reason to raise it.
const (
	MaxReleaseJSONBytes int64  = 1 << 20   // GitHub release metadata
	MaxChecksumBytes    int64  = 64 << 10  // checksums.txt
	MaxArchiveBytes     int64  = 64 << 20  // the compressed archive
	MaxExecutableBytes  int64  = 64 << 20  // the one executable member
	MaxExpandedBytes    uint64 = 128 << 20 // every member's declared size, summed

	RequestTimeout   = 2 * time.Minute // one HTTP request
	OperationTimeout = 3 * time.Minute // a whole upgrade, for the command to apply
	CandidateTimeout = 5 * time.Second // the candidate's version command
)

// Kind classifies an upgrade failure by what a participant can do about it.
type Kind string

const (
	// KindNotRelease: the running binary is not a canonical release build;
	// install a release first. Nothing was fetched.
	KindNotRelease Kind = "not_release"
	// KindDowngrade: the selected release is older than this binary. Nothing
	// was downloaded; only a rollback moves backwards.
	KindDowngrade Kind = "downgrade"
	// KindUnavailable: GitHub could not be reached or answered with a server
	// error. Retrying later is safe.
	KindUnavailable Kind = "unavailable"
	// KindReleaseInvalid: the release, its metadata, an asset, the checksums
	// or the archive is not what a canonical release is. Do not retry
	// blindly; it needs the release owner.
	KindReleaseInvalid Kind = "release_invalid"
	// KindUnsupported: no release artifact exists for this platform.
	KindUnsupported Kind = "unsupported"
	// KindStaging: the candidate could not be written beside the installed
	// binary, usually permissions.
	KindStaging Kind = "staging"
	// KindCandidateInvalid: the staged candidate did not run and report the
	// release's exact version.
	KindCandidateInvalid Kind = "candidate_invalid"
	// KindReplacementFailed: replacement stopped before it was committed and
	// the installed binary and .previous are as they were. Safe to retry once
	// the reason is understood.
	KindReplacementFailed Kind = "replacement_failed"
	// KindPreviousInUse: on Windows, a process still runs from the
	// .previous image, so it cannot be replaced. The displaced binary was
	// restored and .previous is untouched; close old DropinMiner or agent
	// processes and retry.
	KindPreviousInUse Kind = "previous_in_use"
	// KindIncomplete: the replacement neither finished cleanly nor was
	// undone durably — the new binary is installed but the displaced one
	// could not be kept as .previous, or a directory sync failed after a
	// rename. Not a success; Paths names what is where.
	KindIncomplete Kind = "incomplete"
	// KindManualIntervention: a failure and then its recovery both failed.
	// Paths names every surviving copy; put the binary back by hand.
	KindManualIntervention Kind = "manual_intervention"
	// KindNoPrevious: rollback found no .previous to restore.
	KindNoPrevious Kind = "no_previous"
	// KindPreviousInvalid: .previous is not a bounded regular file whose
	// copy reports a release version different from the current one.
	KindPreviousInvalid Kind = "previous_invalid"
)

// PreviousInUseMessage is what a participant is told for KindPreviousInUse.
const PreviousInUseMessage = "an older DropinMiner process is still using the previous executable; close old DropinMiner or agent processes and retry"

// Error carries a Kind; its message is diagnostic only. Paths, when set,
// names every file a participant may need to find after a replacement did
// not end cleanly.
type Error struct {
	Kind  Kind
	Err   error
	Paths []string
}

func (e *Error) Error() string { return "selfupdate: " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func failure(kind Kind, err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	return &Error{Kind: kind, Err: err}
}

// Retryable reports whether the same operation may succeed later with
// nothing repaired by hand.
func (k Kind) Retryable() bool {
	switch k {
	case KindUnavailable, KindPreviousInUse, KindReplacementFailed:
		return true
	}
	return false
}

// errorPreservesPath reports whether err is a typed error that names path in
// its Paths: a copy a participant has been told survives, and that no
// cleanup may remove.
func errorPreservesPath(err error, path string) bool {
	var typed *Error
	if path == "" || !errors.As(err, &typed) {
		return false
	}
	for _, p := range typed.Paths {
		if p == path {
			return true
		}
	}
	return false
}

// KindOf is the Kind of err, or "" when err carries none.
func KindOf(err error) Kind {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Kind
	}
	return ""
}
