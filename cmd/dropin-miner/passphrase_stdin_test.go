package main

// Batch-1 T4: the confirm/passphrase stdin swallow. wallet send used to
// build TWO independent bufio.Readers over the same stdin — one for
// "type yes to send", a second one inside walletPassphrase — so whatever
// the first buffered past its line was invisible to the second. A pipe
// delivers "yes\n<passphrase>\n" as one chunk, not one line at a time, so
// this failed on exactly the non-interactive path the whole feature exists
// for: a script that cannot use a terminal.
//
// This drives the full cmdWallet -> walletSend -> walletPassphrase path
// with a single strings.Reader carrying both lines in one chunk — not
// walletPassphrase alone — because a test that called walletPassphrase
// directly would never exercise the buffering bug: the bug is in what the
// FIRST reader (the confirmation prompt) does to bytes the SECOND one
// needs, and there is no first reader without walletSend's own prompt.

import (
	"bytes"
	"strings"
	"testing"
)

func TestSendAcceptsAPipedConfirmationThenPassphrase(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9,
		balance: "1000000", appearAfter: 1,
	})
	dir := walletScratchDir(t)
	initEnv := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut, initEnv); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	// send WITHOUT -yes and WITHOUT the passphrase env var: the caller
	// pipes both the confirmation ("yes") and the passphrase on stdin, in
	// one chunk, exactly as a script would that cannot use a terminal.
	out.Reset()
	errOut.Reset()
	stdin := strings.NewReader("yes\np-test-1\n")
	code := cmdWallet([]string{"send", "-dir", dir, "-node", node.srv.URL,
		"-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1000"},
		stdin, &out, &errOut, noEnv)
	if code != exitOK {
		t.Fatalf("send with correct piped passphrase failed (exit %d); the confirmation reader swallowed the passphrase line\nstdout:\n%s\nstderr:\n%s",
			code, out.String(), errOut.String())
	}
}

// The wrong passphrase, still piped in one chunk alongside the
// confirmation, must still be refused — the fix must not make
// walletPassphrase accept whatever is left over indiscriminately.
func TestSendRejectsAWrongPipedPassphrase(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9,
		balance: "1000000", appearAfter: 1,
	})
	dir := walletScratchDir(t)
	initEnv := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut, initEnv); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	stdin := strings.NewReader("yes\nwrong-passphrase\n")
	code := cmdWallet([]string{"send", "-dir", dir, "-node", node.srv.URL,
		"-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1000"},
		stdin, &out, &errOut, noEnv)
	if code == exitOK {
		t.Fatalf("send accepted a wrong piped passphrase")
	}
}
