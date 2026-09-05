#!/bin/sh
# dropin-miner bootstrap — the one command that installs everything.
#
#   curl -fsSL https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.sh | sh
#
# What it does, in order:
#   1. get a binary: the latest GitHub release for this OS/arch, checksum
#      verified — or, while no release is tagged, a shallow clone built with
#      the machine's Go
#   2. hand off to scripts/setup.sh, which asks the questions: config,
#      enrollment, payout address, join, shell profile, coding agents
#
# It never uses sudo, writes only under $HOME (binary in ~/.tokendrop/bin,
# source in ~/.tokendrop/src), and reattaches the terminal for setup's
# questions — the `curl | sh` pipe is not the terminal.
#
# Env knobs (all optional):
#   TOKENDROP_INSTALL_REF          branch/tag to clone when building (default main)
#   TOKENDROP_INSTALL_REPO         repo URL
#   TOKENDROP_INSTALL_NO_SETUP=1   stop after the binary; print the next command
set -eu

REPO="${TOKENDROP_INSTALL_REPO:-https://github.com/twilight-project/dropin-miner}"
REF="${TOKENDROP_INSTALL_REF:-main}"
HOME_DIR="${TOKENDROP_HOME:-$HOME/.tokendrop}"
BIN_DIR="$HOME_DIR/bin"
SRC_DIR="$HOME_DIR/src/dropin-miner"
API="https://api.github.com/repos/twilight-project/dropin-miner/releases/latest"

say(){ printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die(){ printf '\nERROR: %s\n' "$*" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || die "curl is required"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in linux|darwin) ;; *) die "unsupported OS: $OS (on Windows use scripts/install.ps1, or WSL)";; esac
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture: $ARCH" ;;
esac

BIN=""
TAG=$(curl -fsSL "$API" 2>/dev/null | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true)
if [ -n "$TAG" ]; then
  say "Latest release: $TAG — downloading for ${OS}/${ARCH}"
  BASE="$REPO/releases/download/$TAG"
  NAME="dropin-miner_${TAG#v}_${OS}_${ARCH}.tar.gz"
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  ( cd "$TMP" && curl -fsSLO "$BASE/$NAME" && curl -fsSLO "$BASE/checksums.txt" ) || die "download failed: $BASE/$NAME"
  if command -v sha256sum >/dev/null 2>&1; then
    ( cd "$TMP" && sha256sum --ignore-missing -c checksums.txt ) || die "checksum FAILED — do not run what you downloaded"
  elif command -v shasum >/dev/null 2>&1; then
    ( cd "$TMP" && shasum -a 256 --ignore-missing -c checksums.txt ) || die "checksum FAILED — do not run what you downloaded"
  else
    die "no sha256sum/shasum on this machine to verify the download with"
  fi
  mkdir -p "$BIN_DIR"
  ( cd "$TMP" && tar -xzf "$NAME" ) || die "could not unpack $NAME"
  install -m 0755 "$TMP/dropin-miner" "$BIN_DIR/dropin-miner"
  install -m 0755 "$TMP/setup.sh" "$BIN_DIR/dropin-miner-setup.sh" 2>/dev/null || true
  BIN="$BIN_DIR/dropin-miner"
  SETUP="$BIN_DIR/dropin-miner-setup.sh"
else
  say "No release is tagged yet — building from source ($REF)"
  command -v git >/dev/null 2>&1 || die "git is required to build from source (or wait for the first release)"
  command -v go >/dev/null 2>&1 || die "Go 1.25+ is required to build from source: https://go.dev/dl/"
  if [ -d "$SRC_DIR/.git" ]; then
    say "Updating existing checkout at $SRC_DIR"
    ( cd "$SRC_DIR" && git fetch -q origin "$REF" && git checkout -q "$REF" && git pull -q --ff-only origin "$REF" ) || die "could not update $SRC_DIR"
  else
    mkdir -p "$(dirname "$SRC_DIR")"
    git clone -q --depth 1 --branch "$REF" "$REPO" "$SRC_DIR" || die "clone failed"
  fi
  ( cd "$SRC_DIR" && CGO_ENABLED=0 go build -trimpath -o "$BIN_DIR/dropin-miner" ./cmd/dropin-miner ) || die "build failed"
  BIN="$BIN_DIR/dropin-miner"
  SETUP="$SRC_DIR/scripts/setup.sh"
fi
say "Installed $BIN"
"$BIN" version || true

if [ "${TOKENDROP_INSTALL_NO_SETUP:-0}" = 1 ]; then
  printf '\nNext: TOKENDROP_BIN=%s sh %s\n' "$BIN" "$SETUP"
  exit 0
fi
[ -f "$SETUP" ] || die "setup script not found at $SETUP"
# The pipe is not the terminal: give setup the real one for its questions.
if [ -t 1 ] && [ -r /dev/tty ]; then
  TOKENDROP_BIN="$BIN" sh "$SETUP" < /dev/tty
else
  TOKENDROP_BIN="$BIN" sh "$SETUP"
fi
