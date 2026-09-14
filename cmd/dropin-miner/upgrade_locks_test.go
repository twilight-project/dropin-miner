package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestUpgradeHoldsTheGateSetupAndTheExecutablesUpdateLock(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	exe := filepath.Join(home, "bin", "dropin-miner")
	writeFileT(t, exe, "binary")
	writeFileT(t, filepath.Join(home, setupConfigFile), "")

	ex, err := excludeForUpgrade(home, exe, 0)
	if err != nil {
		t.Fatalf("nothing running: %v", err)
	}
	if lockIsFree(t, lifecycleGatePath(home)) {
		t.Error("an upgrade holds the lifecycle gate for its whole transaction")
	}
	if lockIsFree(t, filepath.Join(home, setupLockFile)) {
		t.Error("an upgrade holds setup.lock, so a setup cannot start beside it")
	}
	if lockIsFree(t, exe+updateLockSuffix) {
		t.Error("an upgrade holds the executable's update lock")
	}
	if _, err := excludeForUpgrade(home, exe, 0); !errors.Is(err, errLifecycleBusy) {
		t.Errorf("a second upgrade of the same installation is refused as busy, got %v", err)
	}
	ex.release()
	for _, lock := range []string{lifecycleGatePath(home), filepath.Join(home, setupLockFile), exe + updateLockSuffix} {
		if !lockIsFree(t, lock) {
			t.Errorf("release must let go of %s", lock)
		}
	}
}

func TestUpgradeRefusesARunningSetupOrAnotherUpgradeOfTheSameBinary(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	exe := filepath.Join(root, "shared", "bin", "dropin-miner")
	writeFileT(t, exe, "binary")
	writeFileT(t, filepath.Join(home, setupConfigFile), "")

	release := holdLockFile(t, filepath.Join(home, setupLockFile))
	if _, err := excludeForUpgrade(home, exe, 0); activeOperation(err) != "setup" {
		t.Errorf("a running setup refuses the upgrade, got %v", err)
	}
	release()

	// Another installation upgrading the same binary holds its own gate but
	// the same update lock.
	other := filepath.Join(root, "other-home")
	writeFileT(t, filepath.Join(other, setupConfigFile), "")
	first, err := excludeForUpgrade(other, exe, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	if _, err := excludeForUpgrade(home, exe, 0); activeOperation(err) != "upgrade" {
		t.Errorf("two installations sharing a binary must not replace it at once, got %v", err)
	}
	if !lockIsFree(t, lifecycleGatePath(home)) || !lockIsFree(t, filepath.Join(home, setupLockFile)) {
		t.Error("a refused upgrade holds nothing")
	}

	gate := mustAcquireGate(t, lifecycleGatePath(home))
	first.release()
	if _, err := excludeForUpgrade(home, exe, 0); !errors.Is(err, errLifecycleBusy) {
		t.Errorf("a held gate refuses the upgrade before any other lock, got %v", err)
	}
	gate.release()
}
