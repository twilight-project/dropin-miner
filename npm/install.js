#!/usr/bin/env node
// postinstall: fetch the dropin-miner release binary for this platform.
//
// The npm package carries no binary of its own. Its version is the release
// tag; this script downloads exactly that tag's archive from GitHub
// Releases, verifies it against checksums.txt, and unpacks the binary
// beside this file. Nothing is executed from the download, and no
// credential is involved: the release assets are public.
//
// Set DROPIN_MINER_SKIP_DOWNLOAD=1 to install the wrapper without fetching
// (CI images that mount their own binary), or DROPIN_MINER_BINARY=/path to
// point the wrapper at one already on the machine.
"use strict"
const fs = require("node:fs")
const os = require("node:os")
const path = require("node:path")
const https = require("node:https")
const crypto = require("node:crypto")
const { execFileSync } = require("node:child_process")

const pkg = require("./package.json")
const REPO = "twilight-project/dropin-miner"

function platform() {
  const osName = { darwin: "darwin", linux: "linux", win32: "windows" }[process.platform]
  const arch = { x64: "amd64", arm64: "arm64" }[process.arch]
  if (!osName || !arch) throw new Error(`unsupported platform ${process.platform}/${process.arch}`)
  return { osName, arch }
}

function get(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { "user-agent": "dropin-miner-npm" } }, (res) => {
        if ([301, 302, 307, 308].includes(res.statusCode) && res.headers.location && redirects > 0) {
          res.resume()
          return resolve(get(res.headers.location, redirects - 1))
        }
        if (res.statusCode !== 200) {
          res.resume()
          return reject(new Error(`${url}: HTTP ${res.statusCode}`))
        }
        const chunks = []
        res.on("data", (c) => chunks.push(c))
        res.on("end", () => resolve(Buffer.concat(chunks)))
        res.on("error", reject)
      })
      .on("error", reject)
  })
}

async function main() {
  if (process.env.DROPIN_MINER_SKIP_DOWNLOAD === "1" || process.env.DROPIN_MINER_BINARY) return
  if (pkg.version === "0.0.0") {
    console.log("dropin-miner: development package, no release to download; set DROPIN_MINER_BINARY")
    return
  }
  const { osName, arch } = platform()
  const ext = osName === "windows" ? "zip" : "tar.gz"
  const name = `dropin-miner_${pkg.version}_${osName}_${arch}.${ext}`
  const base = `https://github.com/${REPO}/releases/download/v${pkg.version}`

  const [archive, sums] = await Promise.all([get(`${base}/${name}`), get(`${base}/checksums.txt`)])
  const line = sums.toString("utf8").split("\n").find((l) => l.trim().endsWith(name))
  if (!line) throw new Error(`checksums.txt has no entry for ${name}`)
  const expected = line.trim().split(/\s+/)[0]
  const actual = crypto.createHash("sha256").update(archive).digest("hex")
  if (expected !== actual) throw new Error(`checksum FAILED for ${name} — refusing to unpack`)

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "dropin-miner-"))
  const archivePath = path.join(tmp, name)
  fs.writeFileSync(archivePath, archive)
  if (ext === "zip") {
    execFileSync("powershell", ["-NoProfile", "-Command", `Expand-Archive -Path '${archivePath}' -DestinationPath '${tmp}' -Force`], { stdio: "inherit" })
  } else {
    execFileSync("tar", ["-xzf", archivePath, "-C", tmp], { stdio: "inherit" })
  }
  const bin = osName === "windows" ? "dropin-miner.exe" : "dropin-miner"
  const dest = path.join(__dirname, "bin", bin)
  fs.copyFileSync(path.join(tmp, bin), dest)
  if (osName !== "windows") fs.chmodSync(dest, 0o755)
  fs.rmSync(tmp, { recursive: true, force: true })
  console.log(`dropin-miner ${pkg.version} installed for ${osName}/${arch}`)
}

main().catch((err) => {
  console.error(`dropin-miner: ${err.message}`)
  console.error("You can install the binary another way and set DROPIN_MINER_BINARY to its path.")
  process.exit(1)
})
