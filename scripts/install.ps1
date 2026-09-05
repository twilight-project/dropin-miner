# dropin-miner bootstrap for Windows.
#
#   irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex
#
# Downloads the latest release for this machine, verifies its checksum,
# installs it under $HOME\.tokendrop\bin, puts that directory on the USER
# PATH (agents launched after the next sign-in see it), and prints the
# setup steps. Enrollment, payout and the agents are done by the binary
# itself, interactively, so run the printed commands in this window.
#
# Never elevates. Writes only under $HOME and the user's own PATH entry.
$ErrorActionPreference = "Stop"

$Repo = "twilight-project/dropin-miner"
$HomeDir = if ($env:TOKENDROP_HOME) { $env:TOKENDROP_HOME } else { Join-Path $HOME ".tokendrop" }
$BinDir = Join-Path $HomeDir "bin"

$arch = switch ((Get-CimInstance Win32_Processor).Architecture) { 12 { "arm64" } default { "amd64" } }
$release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
$tag = $release.tag_name
$ver = $tag.TrimStart("v")
$name = "dropin-miner_${ver}_windows_${arch}.zip"
$asset = $release.assets | Where-Object { $_.name -eq $name }
if (-not $asset) { throw "no release asset named $name in $tag" }
$sums = $release.assets | Where-Object { $_.name -eq "checksums.txt" }

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("dropin-miner-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
Write-Host "==> Latest release: $tag — downloading $name"
Invoke-WebRequest $asset.browser_download_url -OutFile (Join-Path $tmp $name)
Invoke-WebRequest $sums.browser_download_url -OutFile (Join-Path $tmp "checksums.txt")

$expected = (Get-Content (Join-Path $tmp "checksums.txt") | Where-Object { $_ -match "\s$([regex]::Escape($name))$" }) -split "\s+" | Select-Object -First 1
$actual = (Get-FileHash (Join-Path $tmp $name) -Algorithm SHA256).Hash.ToLower()
if ($expected -ne $actual) { throw "checksum FAILED for $name — do not run what you downloaded" }

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
Expand-Archive -Path (Join-Path $tmp $name) -DestinationPath $tmp -Force
Copy-Item (Join-Path $tmp "dropin-miner.exe") (Join-Path $BinDir "dropin-miner.exe") -Force
Remove-Item -Recurse -Force $tmp

# Owner-only ACL on the state directory: keys and spooled records live here.
icacls $HomeDir /inheritance:r /grant:r "$env:USERNAME:(OI)(CI)F" | Out-Null

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($userPath -split ";") -notcontains $BinDir) {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$BinDir", "User")
  Write-Host "==> Added $BinDir to your user PATH (new windows and agents will see it)"
}
$env:Path = "$env:Path;$BinDir"

$cfg = Join-Path $HomeDir "tokendrop.toml"
if (-not (Test-Path $cfg)) {
  @"
[[provider]]
name     = "search-router"
upstream = "https://router-api.nyks.dev"

[mining]
enabled   = true
as_url    = "https://minis.nyks.dev"
chain_id  = "twilight-devnet-3"
slot_id   = 3
state_dir = "$($HomeDir -replace '\\','\\')\\state"
spool_dir = "$($HomeDir -replace '\\','\\')\\spool"

[miner]
enabled      = true
intake_dir   = "$($HomeDir -replace '\\','\\')\\intake"
sessions_dir = "$($HomeDir -replace '\\','\\')\\sessions"
"@ | Set-Content -Path $cfg -Encoding UTF8
  Write-Host "==> Wrote $cfg"
}
[Environment]::SetEnvironmentVariable("TOKENDROP_CONFIG", $cfg, "User")
$env:TOKENDROP_CONFIG = $cfg

& (Join-Path $BinDir "dropin-miner.exe") version
Write-Host @"

Installed. Finish in this window, in order:

  1. Generate an enrollment token (expires in 15 minutes, single use):
       https://platform.nyks.dev -> Mining -> Slot 3 -> Generate enrollment token
     then:  dropin-miner enroll -assertion        (paste the token, Enter)
  2. Where to be paid — one of:
       dropin-miner wallet init ; dropin-miner wallet register
       dropin-miner payout set twilight1...
  3. dropin-miner join
  4. dropin-miner agents install
  5. Set your sr- key for your agents:  setx TOKENDROP_API_KEY sr-...
     then restart any open agent.

Cursor and Claude Code run shell commands through PowerShell or Git Bash;
both find dropin-miner on PATH after the next sign-in.
"@
