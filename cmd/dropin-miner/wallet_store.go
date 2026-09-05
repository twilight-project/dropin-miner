package main

// The wallet's on-disk layout: a 0700 directory holding the encrypted
// keyfile and a plaintext sidecar. Pure stdlib — all key material and
// crypto live behind internal/auth's audited surface (ADR-0010).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

const (
	walletKeyFile     = "wallet.key"
	walletSidecarFile = "wallet.pub"

	// walletDirEnv names the wallet directory without repeating -dir on
	// every command. The setup script puts the wallet beside the rest of
	// a participant's files (~/.tokendrop/wallet) rather than in the OS
	// config dir, and without this every documented example would have to
	// carry the path — or, worse, silently look in the wrong place.
	walletDirEnv = "TOKENDROP_WALLET_DIR"
)

// sidecar is the public half, stored in the clear so read-only commands
// (address, register, balance) never need the passphrase.
type sidecar struct {
	Address string `json:"address"`
	PubKey  string `json:"pubkey"` // 33-byte compressed, hex
	Path    string `json:"path"`
}

// defaultWalletDir mirrors the mining state-dir convention.
func defaultWalletDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("wallet: resolve config dir: %w", err)
	}
	return filepath.Join(base, "tokendrop", "wallet"), nil
}

// openWalletDir creates (0700) and validates the wallet directory with
// the same posture as the auth store: no symlink, no group/world access.
func openWalletDir(dir string, getenv func(string) string) (string, error) {
	if dir == "" {
		dir = getenv(walletDirEnv)
	}
	if dir == "" {
		var err error
		if dir, err = defaultWalletDir(); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("wallet: create dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("wallet: stat dir: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return "", errors.New("wallet: dir is a symlink; refusing")
	}
	if !info.IsDir() {
		return "", errors.New("wallet: path is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("wallet: dir is group/world-accessible (%04o); refusing", info.Mode().Perm())
	}
	return dir, nil
}

func writeWalletFile(dir, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("wallet: write %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

func readWalletFile(dir, name string, v any) error {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("wallet: %s is a symlink; refusing", name)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is the validated wallet dir plus a fixed name
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// loadSidecar reads the public half; the error explains the fix.
func loadSidecar(dir string, getenv func(string) string) (*sidecar, string, error) {
	resolved, err := openWalletDir(dir, getenv)
	if err != nil {
		return nil, "", err
	}
	var sc sidecar
	if err := readWalletFile(resolved, walletSidecarFile, &sc); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, resolved, errors.New("wallet: no wallet here yet — run: dropin-miner wallet init")
		}
		return nil, resolved, err
	}
	if _, _, err := auth.DecodeBech32Address(sc.Address); err != nil {
		return nil, resolved, fmt.Errorf("wallet: sidecar address is corrupt: %w", err)
	}
	return &sc, resolved, nil
}
