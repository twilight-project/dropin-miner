# dropin-miner bootstrap for Windows.
#
#   irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex
#
# Downloads the latest release for this machine, verifies its checksum,
# installs it under $HOME\.tokendrop\bin, and hands off to
# `dropin-miner.exe setup`: it restricts the installation directory to you,
# writes the config, runs `connect` (registration, the mining question in
# this window, a wallet if you want one), puts the binary on your USER PATH
# and sets TOKENDROP_CONFIG, and offers the coding agents it finds. No
# enrollment token to generate, no key to paste, no join to run by hand.
#
# A binary from before setup moved into it (v0.2.8 and older) has no `setup`
# command. The capability is probed, never assumed from a version: when
# `dropin-miner.exe setup -h` does not exit 0, this script's own config,
# PATH and connect steps run instead, exactly as they did before.
#
# TOKENDROP_INSTALL_BIN=<path> skips the download and uses that binary
# (air-gapped installs, and the installer's own tests).
#
# Never elevates. Writes only under $HOME and the user's own environment.
$ErrorActionPreference = "Stop"

$Repo = "twilight-project/dropin-miner"
$HomeDir = if ($env:TOKENDROP_HOME) { $env:TOKENDROP_HOME } else { Join-Path $HOME ".tokendrop" }
$BinDir = Join-Path $HomeDir "bin"

if ($env:TOKENDROP_INSTALL_BIN) {
  $exe = $env:TOKENDROP_INSTALL_BIN
  if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { throw "TOKENDROP_INSTALL_BIN is not a file: $exe" }
  Write-Host "==> Using $exe (TOKENDROP_INSTALL_BIN; nothing downloaded)"
} else {
  $arch = switch ((Get-CimInstance Win32_Processor).Architecture) { 12 { "arm64" } default { "amd64" } }
  $release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
  $tag = $release.tag_name
  $ver = $tag.TrimStart("v")
  $name = "dropin-miner_${ver}_windows_${arch}.zip"
  $asset = $release.assets | Where-Object { $_.name -eq $name }
  if (-not $asset) { throw "no release asset named $name in $tag" }
  $sums = $release.assets | Where-Object { $_.name -eq "checksums.txt" }

  # The temporary directory goes on EVERY exit path, not only the happy one.
  # The checksum branch below throws, and what a bare success-path cleanup
  # leaves in %TEMP% is the one file the script has just called untrustworthy
  # (#85). $tmp is named before the try so finally can always see it;
  # SilentlyContinue covers the case where New-Item itself failed.
  $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("dropin-miner-" + [guid]::NewGuid())
  try {
    New-Item -ItemType Directory -Path $tmp | Out-Null
    Write-Host "==> Latest release: $tag - downloading $name"
    Invoke-WebRequest $asset.browser_download_url -OutFile (Join-Path $tmp $name)
    Invoke-WebRequest $sums.browser_download_url -OutFile (Join-Path $tmp "checksums.txt")

    $expected = (Get-Content (Join-Path $tmp "checksums.txt") | Where-Object { $_ -match "\s$([regex]::Escape($name))$" }) -split "\s+" | Select-Object -First 1
    $actual = (Get-FileHash (Join-Path $tmp $name) -Algorithm SHA256).Hash.ToLower()
    if ($expected -ne $actual) { throw "checksum FAILED for $name - do not run what you downloaded" }

    New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
    Expand-Archive -Path (Join-Path $tmp $name) -DestinationPath $tmp -Force
    Copy-Item (Join-Path $tmp "dropin-miner.exe") (Join-Path $BinDir "dropin-miner.exe") -Force
  } finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
  $exe = Join-Path $BinDir "dropin-miner.exe"
}

# The capability contract: `setup -h` exits 0 on a binary that has setup. An
# older binary answers "unknown command" on stderr, which Windows PowerShell
# would turn into a terminating error under "Stop" - so the probe runs under
# "Continue" and is judged by its exit code alone.
$preference = $ErrorActionPreference
$ErrorActionPreference = "Continue"
& $exe setup -h > $null 2> $null
$probe = $LASTEXITCODE
$ErrorActionPreference = $preference

if ($probe -eq 0) {
  & $exe setup
  if ($LASTEXITCODE -ne 0) { throw "setup failed (exit $LASTEXITCODE)" }
} else {
# -- a binary without setup (v0.2.8 and older) -------------------------------
# Kept as it was until v0.3.0 is the latest release; then this branch goes.
Write-Host "Recent agent context may accompany search to the Twilight search router as part of the trajectory/search product."
Write-Host "TOKENDROP_TRACE=off disables trace transmission. Mining/AS receives metadata observations only."

# Owner-only ACL on the state directory: keys and spooled records live here.
icacls $HomeDir /inheritance:r /grant:r "$env:USERNAME:(OI)(CI)F" | Out-Null

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($userPath -split ";") -notcontains $BinDir) {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$BinDir", "User")
  Write-Host "==> Added $BinDir to your user PATH (new windows and agents will see it)"
}
$env:Path = "$env:Path;$BinDir"

# -- config -----------------------------------------------------------------
# [mining] never says enabled = true unconditionally: a terminal answers
# connect's own question below, and that answer is the decision. Only a
# genuinely non-interactive run (input redirected, TOKENDROP_MINING=1) writes
# it here - connect persists whatever this ends up saying either way, so
# this always ends with a decision on file, never an absent one.
$cfg = Join-Path $HomeDir "tokendrop.toml"
$platformUrl = if ($env:TOKENDROP_PLATFORM_URL) { $env:TOKENDROP_PLATFORM_URL } else { "https://platform.nyks.dev" }
$agentsApiUrl = if ($env:TOKENDROP_AGENTS_API_URL) { $env:TOKENDROP_AGENTS_API_URL } else { "https://agents-v1.nyks.dev" }
$asUrl = if ($env:TOKENDROP_AS_URL) { $env:TOKENDROP_AS_URL } else { "https://rewards.nyks.dev" }
$chain = if ($env:TOKENDROP_CHAIN) { $env:TOKENDROP_CHAIN } else { "twilight-testnet-1" }
$slot = if ($env:TOKENDROP_SLOT) { $env:TOKENDROP_SLOT } else { "3" }
$router = if ($env:TOKENDROP_ROUTER_URL) { $env:TOKENDROP_ROUTER_URL } else { "https://router-api.nyks.dev" }

if (-not (Test-Path $cfg)) {
  $miningLines = ""
  if ([Console]::IsInputRedirected -and $env:TOKENDROP_MINING -eq "1") {
    $miningLines = "enabled   = true`r`n"
    if ($env:TOKENDROP_PAYOUT_ADDRESS) {
      $miningLines += "payout_address = `"$($env:TOKENDROP_PAYOUT_ADDRESS)`"`r`n"
    }
  }
  @"
[[provider]]
name     = "search-router"
upstream = "$router"

[platform]
base_url       = "$platformUrl"
agents_api_url = "$agentsApiUrl"

[mining]
${miningLines}as_url    = "$asUrl"
chain_id  = "$chain"
slot_id   = $slot
state_dir = "$($HomeDir -replace '\\','\\')\\state"
spool_dir = "$($HomeDir -replace '\\','\\')\\spool"

[miner]
enabled      = true
intake_dir   = "$($HomeDir -replace '\\','\\')\\intake"
sessions_dir = "$($HomeDir -replace '\\','\\')\\sessions"
"@ | Set-Content -Path $cfg -Encoding UTF8
  Write-Host "==> Wrote $cfg"
} else {
  Write-Host "==> Config already exists: $cfg (left as is)"
}
[Environment]::SetEnvironmentVariable("TOKENDROP_CONFIG", $cfg, "User")
$env:TOKENDROP_CONFIG = $cfg

& $exe version

# -- connect ----------------------------------------------------------------
# Asks the mining question in this window when one is present, creates or
# takes a wallet, then registers and stores the key it mints (the answer
# just given hints the claim page's mining pre-tick). Prints the claim
# URL and waits a few minutes for it; if nobody has claimed by then it says
# so and exits - the claim still works whenever it happens, picked up
# automatically by your first real search.
Write-Host "==> Connecting"
$env:TOKENDROP_WALLET_DIR = Join-Path $HomeDir "wallet"
& $exe connect -config $cfg
if ($LASTEXITCODE -ne 0) { throw "connect failed (exit $LASTEXITCODE)" }

Write-Host @"

Installed and connected.

  1. If a claim URL was printed above and nobody has visited it yet, do
     that whenever you're ready - nothing else here depends on timing it.
     Once claimed, mining (if you said yes) enrolls and declares a payout
     on its own, from the next search.
  2. dropin-miner agents install     (writes a skill + hooks for the coding
                                       agents found on this machine)
  3. Restart any open agent, then search as usual.
  4. Check in any time: dropin-miner status / doctor / payout show

Cursor and Claude Code run shell commands through PowerShell or Git Bash;
both find dropin-miner on PATH after the next sign-in.
"@
}
