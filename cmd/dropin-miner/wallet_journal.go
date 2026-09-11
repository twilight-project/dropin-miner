package main

// The send journal (REL-15): a transaction is written to disk before it
// is broadcast, so a lost response is resolvable — the next `wallet
// send` or `wallet balance` can ask the node whether it actually landed
// — rather than the only recovery being "sign again and hope the
// sequence number saved you," which it does not: a node that accepted
// the first attempt and merely failed to answer accepts the second one
// too, and the participant is paid twice for one intended send.
//
// No new payment is ever constructed while a journal is pending. The
// journal's own file is written through writeWalletFile — the same
// CreateTemp+Chmod+Sync+rename atomic writer wallet.key and wallet.pub
// already use, owner-only, in the wallet directory.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	pendingTxFile    = "pending_tx.json"
	pendingTxVersion = 1
)

// pendingTx is the journal's on-disk shape. TxRawB64 is the exact bytes
// that were signed and broadcast — a re-broadcast after a lost response
// resends THESE bytes, never a fresh signature, so a rerun can never
// pay twice under two different transaction ids for one intended send.
type pendingTx struct {
	Version   int    `json:"version"`
	Hash      string `json:"hash"`
	TxRawB64  string `json:"tx_raw_b64"`
	To        string `json:"to"`
	Amount    string `json:"amount"`
	Denom     string `json:"denom"`
	ChainID   string `json:"chain_id"`
	Sequence  uint64 `json:"sequence"`
	CreatedAt string `json:"created_at"`
}

func (p *pendingTx) txRaw() ([]byte, error) {
	return base64.StdEncoding.DecodeString(p.TxRawB64)
}

// writePendingTx journals a transaction BEFORE it is broadcast — the
// ordering §4.3 requires, and the reason this is called from walletSend
// ahead of c.broadcast rather than after.
func writePendingTx(dir string, p pendingTx) error {
	p.Version = pendingTxVersion
	return writeWalletFile(dir, pendingTxFile, &p)
}

// loadPendingTx returns nil, nil when no journal exists — the ordinary
// state between sends.
func loadPendingTx(dir string) (*pendingTx, error) {
	var p pendingTx
	if err := readWalletFile(dir, pendingTxFile, &p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if p.Version != pendingTxVersion || p.Hash == "" || p.TxRawB64 == "" {
		return nil, fmt.Errorf("wallet: %s is present but does not parse as a pending transaction", pendingTxFile)
	}
	return &p, nil
}

func removePendingTx(dir string) error {
	err := os.Remove(filepath.Join(dir, pendingTxFile)) // #nosec G703 -- dir is the validated wallet dir, pendingTxFile is a fixed name
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// pendingOutcome is what resolvePendingTx found.
type pendingOutcome int

const (
	// pendingNone: no journal existed. A new payment may proceed.
	pendingNone pendingOutcome = iota
	// pendingResolved: a journal existed and is now settled (confirmed
	// or rejected) and removed. For `send`, this invocation still
	// refuses to build a NEW payment — §4.3: report it, and let the
	// participant run send again for a second transfer, in a separate
	// invocation, so one send's outcome is never conflated with
	// another's.
	pendingResolved
	// pendingUnresolved: a journal exists and the node still does not
	// know its hash; it was re-broadcast (same bytes, no new
	// signature) and remains journaled. A new payment must wait.
	pendingUnresolved
	// pendingCheckFailed: the node could not be asked. The journal is
	// left exactly as it was; nothing here is destructive on failure.
	pendingCheckFailed
)

// resolvePendingTx is the read-the-journal-first step both `wallet
// send` and `wallet balance` perform. It never constructs a new
// transaction and never regenerates a signature — only re-broadcasts
// the bytes already on disk, if the node has not seen them.
func resolvePendingTx(ctx context.Context, c *rpcClient, dir string, stdout, stderr io.Writer) pendingOutcome {
	p, err := loadPendingTx(dir)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: pending transaction record:", err)
		return pendingCheckFailed
	}
	if p == nil {
		return pendingNone
	}

	res, found, confirmed, err := c.queryTxOnce(ctx, p.Hash)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: a previous send (%s) is still unresolved and could not be checked: %v\n", p.Hash, err)
		return pendingCheckFailed
	}
	if confirmed {
		if res.TxResult.Code != 0 {
			fmt.Fprintf(stdout, "a previous send (%s) is now known: FAILED at height %s (code %d): %s\n",
				p.Hash, res.Height, res.TxResult.Code, res.TxResult.Log)
		} else {
			fmt.Fprintf(stdout, "a previous send (%s) is now known: confirmed in block %s\n", p.Hash, res.Height)
		}
		if err := removePendingTx(dir); err != nil {
			fmt.Fprintln(stderr, "dropin-miner: could not clear the resolved pending transaction:", err)
			return pendingCheckFailed
		}
		return pendingResolved
	}
	_ = found // the node not knowing the hash and a transport "not found" both mean re-broadcast

	txRaw, err := p.txRaw()
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: pending transaction record is corrupt:", err)
		return pendingCheckFailed
	}
	// Never a fresh signature: these are the exact bytes signed the
	// first time. broadcast's own hash-match check (REL-17) still runs
	// against them, so a node reporting anything but agreement on this
	// same hash leaves the journal in place rather than being trusted.
	if _, err := c.broadcast(ctx, txRaw); err != nil {
		fmt.Fprintf(stdout, "a previous send (%s) is still unresolved; re-sent the same signed transaction, outcome still unknown\n", p.Hash)
		return pendingUnresolved
	}
	fmt.Fprintf(stdout, "a previous send (%s) is still unresolved; re-sent the same signed transaction, now accepted — check again to confirm\n", p.Hash)
	return pendingUnresolved
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
