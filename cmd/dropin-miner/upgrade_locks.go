package main

// What an upgrade or rollback holds for its whole transaction.
//
// The installation's lifecycle gate, from before anything is fetched or
// inspected until the replacement has committed or been undone, so no setup,
// connect or flush admission crosses it; setup.lock, so a setup already
// running refuses the upgrade rather than having its binary replaced under
// it; and the executable's own update lock, <resolved binary>.update.lock,
// taken after the gate. The gate is keyed on the installation, so two
// installations that share one binary would hold different gates; the update
// lock beside the binary is what serializes their replacements of it.

import (
	"path/filepath"
	"time"
)

const updateLockSuffix = ".update.lock"

// excludeForUpgrade takes, in lock order, the gate of home, setup.lock and
// executable's update lock, and keeps them. A held gate is errLifecycleBusy;
// a running setup, or another upgrade of the same binary, is a
// lifecycleActiveError. Nothing is held after a refusal.
func excludeForUpgrade(home, executable string, wait time.Duration) (*lifecycleExclusion, error) {
	gate, err := acquireLifecycleGate(lifecycleGatePath(home), wait)
	if err != nil {
		return nil, err
	}
	ex := &lifecycleExclusion{home: home, gate: gate}
	if err := ex.hold("setup", filepath.Join(home, setupLockFile)); err != nil {
		ex.release()
		return nil, err
	}
	if err := ex.hold("upgrade", executable+updateLockSuffix); err != nil {
		ex.release()
		return nil, err
	}
	return ex, nil
}
