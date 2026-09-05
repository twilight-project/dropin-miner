# dropin-miner (npm wrapper)

```bash
npx dropin-miner agents install
```

This package downloads the `dropin-miner` release binary for your platform
on install, verifies it against the release checksums, and forwards every
argument to it. The binary, its commands and its configuration are
documented in the main repository:
https://github.com/twilight-project/dropin-miner

Environment:

- `DROPIN_MINER_BINARY=/path` use a binary already on the machine
- `DROPIN_MINER_SKIP_DOWNLOAD=1` install the wrapper without fetching

The package version is the release tag it fetches.
