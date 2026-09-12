package main

// The wallet writer delegates to the shared durable primitive.
//
// What pkg/fsx guarantees — that a reader sees either the whole old file
// or the whole new one, and that the directory entry is durable — is
// proven there, by its own TestFailureStages against an injected failure
// at each stage. Re-proving it here would be testing somebody else's
// package through a keyhole. What is ours to prove is the delegation
// itself: that every wallet write actually goes through that primitive,
// and that when it refuses, the refusal reaches the caller with the wallet
// context attached and nothing has been left behind on disk.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

// errWriterRefused is what the injected writer returns, shaped like the
// real thing: fsx reports a stage, so a failure that never reached
// publication is distinguishable from one that may have.
var errWriterRefused = &fsx.StageError{Stage: "write", Err: errors.New("synthetic: the durable writer refused")}

func TestWalletWriteGoesThroughTheDurableWriterAndPropagatesItsFailure(t *testing.T) {
	dir := walletScratchDir(t)
	const name = walletSidecarFile
	final := filepath.Join(dir, name)

	// An existing file the failed write must not disturb. Wallet material
	// is the one thing on this machine with no second copy, so a writer
	// that fails must leave what was already there exactly as it was.
	existing := []byte("{\n  \"address\": \"twilight1previous\"\n}\n")
	if err := os.WriteFile(final, existing, 0o600); err != nil {
		t.Fatal(err)
	}

	called := 0
	var gotDir, gotName string
	var gotMode os.FileMode
	restore := writeWalletAtomic
	writeWalletAtomic = func(dir, name string, data []byte, mode os.FileMode) error {
		called++
		gotDir, gotName, gotMode = dir, name, mode
		if len(data) == 0 {
			t.Error("the writer was handed no bytes")
		}
		return errWriterRefused
	}
	defer func() { writeWalletAtomic = restore }()

	err := writeWalletFile(dir, name, &sidecar{Address: "twilight1next", PubKey: "02ab", Path: "m/44'/118'/0'/0/0"})
	if err == nil {
		t.Fatal("a refusing durable writer was reported as a successful write")
	}
	// Delegation: the primitive was called, once, with what it was given.
	if called != 1 {
		t.Fatalf("the durable writer was called %d times, want exactly 1", called)
	}
	if gotDir != dir || gotName != name {
		t.Errorf("wrote to %s/%s, want %s/%s", gotDir, gotName, dir, name)
	}
	if gotMode != 0o600 {
		t.Errorf("mode = %04o, want 0600 — wallet files are owner-only", gotMode)
	}
	// Propagation: the wallet context is added and the cause survives it,
	// so a caller can still classify the stage it failed at.
	if want := "wallet: write " + name + ":"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q does not carry the %q prefix", err, want)
	}
	if !errors.Is(err, errWriterRefused) {
		t.Errorf("the writer's own error did not survive wrapping: %v", err)
	}
	var stage *fsx.StageError
	if !errors.As(err, &stage) || stage.Published {
		t.Errorf("the failure stage was lost or reported as published: %v", err)
	}
	// Nothing was written behind the primitive's back.
	if got, err := os.ReadFile(final); err != nil { // #nosec G304 -- final is this test's own TempDir plus a fixed name
		t.Fatalf("the file that was already there is gone: %v", err)
	} else if string(got) != string(existing) {
		t.Errorf("the existing file was modified by a failed write:\n got %q\nwant %q", got, existing)
	}
}

// The same, with no file there to begin with: a failed write must not
// leave a partial one, and must not create the final name at all.
func TestWalletWriteFailureCreatesNoFile(t *testing.T) {
	dir := walletScratchDir(t)
	restore := writeWalletAtomic
	writeWalletAtomic = func(string, string, []byte, os.FileMode) error { return errWriterRefused }
	defer func() { writeWalletAtomic = restore }()

	if err := writeWalletFile(dir, pendingTxFile, &pendingTx{Version: pendingTxVersion, Hash: "ABC"}); err == nil {
		t.Fatal("a refusing durable writer was reported as a successful write")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("a failed write left %q in the wallet directory", e.Name())
	}
}

// And the ordinary path still lands: the seam is a test seam, not a
// second implementation. A real write through the real primitive produces
// a file the reader can load back.
func TestWalletWriteThroughTheRealPrimitiveRoundTrips(t *testing.T) {
	dir := walletScratchDir(t)
	want := sidecar{Address: "twilight1roundtrip", PubKey: "02cd", Path: "m/44'/118'/0'/0/0"}
	if err := writeWalletFile(dir, walletSidecarFile, &want); err != nil {
		t.Fatal(err)
	}
	var got sidecar
	if err := readWalletFile(dir, walletSidecarFile, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip changed the sidecar:\n got %+v\nwant %+v", got, want)
	}
	if posixModes {
		info, err := os.Stat(filepath.Join(dir, walletSidecarFile))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %04o, want 0600", perm)
		}
	}
}
