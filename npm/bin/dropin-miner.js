#!/usr/bin/env node
// The npm entry point: hand every argument to the real binary and return
// its exit code. Nothing is interpreted here, so `npx dropin-miner search q`
// is exactly `dropin-miner search q`.
//
// One thing is added for the child: DROPIN_MINER_LAUNCH=npm:<kind>, where
// kind is global, local or unknown. `dropin-miner setup` writes this
// binary's path into every hook and skill, so it refuses to run from a copy
// npm or a project will discard — an exec cache or a project's own
// node_modules. Only this launcher knows which kind of install it is, and it
// works that out from paths alone; npm itself is never run to ask.
"use strict"
const path = require("node:path")
const { spawnSync } = require("node:child_process")

// npmGlobalRoots is where `npm root -g` would point, for the prefixes this
// process can know without running npm: one exported in the environment
// (npm_config_prefix), and the one implied by the node binary itself —
// <prefix>/bin/node on POSIX, <prefix>\node.exe on Windows. The global
// package directory is <prefix>/lib/node_modules on POSIX and
// <prefix>\node_modules on Windows.
function npmGlobalRoots(execPath, env, platform) {
  const p = platform === "win32" ? path.win32 : path.posix
  const prefixes = []
  if (env.npm_config_prefix) prefixes.push(env.npm_config_prefix)
  prefixes.push(platform === "win32" ? p.dirname(execPath) : p.dirname(p.dirname(execPath)))
  return prefixes.map((prefix) =>
    platform === "win32" ? p.join(prefix, "node_modules") : p.join(prefix, "lib", "node_modules"),
  )
}

// launchKind classifies the package directory (the directory holding this
// package's package.json) as a global install, a local one, or neither.
//
// A prefix set in ~/.npmrc (prefix=~/.npm-global, the usual no-sudo setup)
// is invisible from here without running npm, so the global layout itself is
// the second witness: a global install puts its command shim at the prefix —
// <prefix>/bin/dropin-miner beside <prefix>/lib/node_modules on POSIX,
// <prefix>\dropin-miner.cmd beside <prefix>\node_modules on Windows — where a
// project install puts it under node_modules/.bin instead.
function launchKind(packageDir, execPath, env, platform, exists) {
  const p = platform === "win32" ? path.win32 : path.posix
  const norm = (s) => {
    const r = p.resolve(s)
    return platform === "win32" ? r.toLowerCase() : r
  }
  const modules = p.dirname(packageDir)
  for (const root of npmGlobalRoots(execPath, env, platform)) {
    if (norm(root) === norm(modules)) return "global"
  }
  if (p.basename(modules) !== "node_modules") return "unknown"
  const name = p.basename(packageDir)
  if (platform === "win32") {
    if (exists(p.join(p.dirname(modules), name + ".cmd"))) return "global"
  } else if (p.basename(p.dirname(modules)) === "lib") {
    if (exists(p.join(p.dirname(p.dirname(modules)), "bin", name))) return "global"
  }
  return "local"
}

module.exports = { launchKind, npmGlobalRoots }

if (require.main === module) {
  const bin =
    process.env.DROPIN_MINER_BINARY ||
    path.join(__dirname, process.platform === "win32" ? "dropin-miner.exe" : "dropin-miner")

  const exists = (f) => {
    try {
      require("node:fs").lstatSync(f)
      return true
    } catch {
      return false
    }
  }
  const kind = launchKind(path.dirname(__dirname), process.execPath, process.env, process.platform, exists)
  const env = Object.assign({}, process.env, { DROPIN_MINER_LAUNCH: "npm:" + kind })
  const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit", env })
  if (result.error) {
    console.error(`dropin-miner: cannot run ${bin}: ${result.error.message}`)
    console.error("Reinstall the package, or set DROPIN_MINER_BINARY to a dropin-miner binary.")
    process.exit(1)
  }
  process.exit(result.status === null ? 1 : result.status)
}
