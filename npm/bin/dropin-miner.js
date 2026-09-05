#!/usr/bin/env node
// The npm entry point: hand every argument to the real binary and return
// its exit code. Nothing is interpreted here, so `npx dropin-miner search q`
// is exactly `dropin-miner search q`.
"use strict"
const path = require("node:path")
const { spawnSync } = require("node:child_process")

const bin =
  process.env.DROPIN_MINER_BINARY ||
  path.join(__dirname, process.platform === "win32" ? "dropin-miner.exe" : "dropin-miner")

const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" })
if (result.error) {
  console.error(`dropin-miner: cannot run ${bin}: ${result.error.message}`)
  console.error("Reinstall the package, or set DROPIN_MINER_BINARY to a dropin-miner binary.")
  process.exit(1)
}
process.exit(result.status === null ? 1 : result.status)
