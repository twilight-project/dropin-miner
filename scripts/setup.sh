#!/bin/sh
# dropin-miner setup — everything after the binary, asked as it goes.
#
#   config, enrollment, where you get paid, the first join, your shell
#   profile, your coding agents, and a first flush. No service: the miner
#   runs inside your agents' tool calls and nowhere else.
#
# Env knobs (all optional):
#   TOKENDROP_BIN        the dropin-miner binary (default: ./bin/dropin-miner, then PATH)
#   TOKENDROP_HOME       state directory (default ~/.tokendrop — shared with a proxy if you run one)
#   TOKENDROP_SLOT / TOKENDROP_CHAIN / TOKENDROP_AS_URL / TOKENDROP_ROUTER_URL
#                        the Slot to mine for (defaults: 3, twilight-devnet-3,
#                        https://minis.nyks.dev, https://router-api.nyks.dev)
set -eu

SLOT="${TOKENDROP_SLOT:-3}"
CHAIN="${TOKENDROP_CHAIN:-twilight-devnet-3}"
AS_URL="${TOKENDROP_AS_URL:-https://minis.nyks.dev}"
ROUTER="${TOKENDROP_ROUTER_URL:-https://router-api.nyks.dev}"
HOME_DIR="${TOKENDROP_HOME:-$HOME/.tokendrop}"

say(){ printf '\n\033[1m%s\033[0m\n' "$*"; }
die(){ printf '\nERROR: %s\n' "$*" >&2; exit 1; }

# ── 1. the binary ────────────────────────────────────────────────────────────
BIN="${TOKENDROP_BIN:-}"
if [ -z "$BIN" ]; then
  if [ -x ./bin/dropin-miner ]; then BIN=$(pwd)/bin/dropin-miner
  elif command -v dropin-miner >/dev/null 2>&1; then BIN=$(command -v dropin-miner)
  elif [ -f ./go.mod ] && command -v go >/dev/null 2>&1; then
    say "Building from this checkout"
    ( make build ) || die "build failed"
    BIN=$(pwd)/bin/dropin-miner
  else
    die "No dropin-miner binary. Download the release for this machine and point TOKENDROP_BIN at it:
    https://github.com/twilight-project/dropin-miner/releases
    TOKENDROP_BIN=/path/to/dropin-miner $0"
  fi
fi
[ -x "$BIN" ] || die "not executable: $BIN"
say "Using binary: $BIN"; "$BIN" version 2>/dev/null || true

# ── 2. directories (0700 matters: keys and spooled records live here) ────────
mkdir -p "$HOME_DIR/state" "$HOME_DIR/spool" "$HOME_DIR/intake" "$HOME_DIR/sessions"
chmod 700 "$HOME_DIR" "$HOME_DIR/state" "$HOME_DIR/spool" "$HOME_DIR/intake" "$HOME_DIR/sessions"

# ── 3. config ────────────────────────────────────────────────────────────────
CFG="$HOME_DIR/tokendrop.toml"
if [ -f "$CFG" ] && grep -q '^\[miner\]' "$CFG"; then
  say "Config already has a [miner] block: $CFG (left as is)"
else
  cat > "$CFG" <<TOML
[[provider]]
name     = "search-router"
upstream = "$ROUTER"   # the GATEWAY, not the verification API

[mining]
enabled   = true
as_url    = "$AS_URL"
chain_id  = "$CHAIN"
slot_id   = $SLOT
# target_epoch deliberately unset — flush asks the AS which epoch to join.
state_dir = "$HOME_DIR/state"
spool_dir = "$HOME_DIR/spool"

[miner]
enabled      = true
intake_dir   = "$HOME_DIR/intake"
sessions_dir = "$HOME_DIR/sessions"
TOML
  chmod 600 "$CFG"
  say "Wrote $CFG"
fi

# ── 4. enroll ────────────────────────────────────────────────────────────────
if [ -f "$HOME_DIR/state/refresh.token" ]; then
  say "Already enrolled — skipping."
else
  cat <<'MSG'

Generate your enrollment token NOW (not earlier — it expires in 15 minutes
and is single-use):

    https://platform.nyks.dev  ->  Mining  ->  Slot 3  ->  Generate enrollment token

Paste it below, then press Enter.
MSG
  "$BIN" enroll -assertion -config "$CFG" || die "enrollment failed (expired token? generate a fresh one and re-run)"
fi

# ── 5. where you get paid ────────────────────────────────────────────────────
say "Where you get paid"
WALLET_DIR="$HOME_DIR/wallet"
if [ -f "$WALLET_DIR/wallet.key" ]; then
  PAYOUT=$("$BIN" wallet address -dir "$WALLET_DIR")
  PAYOUT_SOURCE=wallet
  echo "Using the wallet already in $WALLET_DIR: $PAYOUT"
  "$BIN" wallet register -dir "$WALLET_DIR" -config "$CFG"
else
  cat <<'MSG'
This machine can hold your rewards for you, or they can go to a wallet you
already run (Keplr, say).

  [1] Make a wallet here          — nothing to copy, nothing to mistype.
                                    Prints a 24-word recovery phrase ONCE:
                                    have paper ready.
  [2] Paste an address I own      — twilight1…

MSG
  printf 'Choose [1/2]: '
  read -r CHOICE || die "stdin ended before you chose. Re-run — enrollment is already done and will be skipped."
  case "$CHOICE" in
    1|"")
      "$BIN" wallet init -dir "$WALLET_DIR" || die "wallet init failed (run this in a real terminal — the recovery phrase must not go into a log)"
      PAYOUT=$("$BIN" wallet address -dir "$WALLET_DIR")
      PAYOUT_SOURCE=wallet
      printf '\nWrite the 24 words down NOW if you have not. Press Enter when they are on paper: '
      read -r _ || true
      "$BIN" wallet register -dir "$WALLET_DIR" -config "$CFG"
      ;;
    2)
      printf 'Your twilight1… address: '
      read -r PAYOUT || PAYOUT=""
      [ -n "$PAYOUT" ] || die "no address given. Re-run — enrollment is already done and will be skipped."
      case "$PAYOUT" in twilight1*) ;; *) die "that does not look like a twilight1… address" ;; esac
      PAYOUT_SOURCE=pasted
      "$BIN" payout set "$PAYOUT" -config "$CFG"
      ;;
    *) die "answer 1 or 2" ;;
  esac
fi

# ── 6. join, so the first hour counts ────────────────────────────────────────
say "Joining the current target epoch"
"$BIN" join -config "$CFG" || echo "(join did not succeed now; every flush retries it)"

# ── 7. shell profile ─────────────────────────────────────────────────────────
BIN_DIR=$(cd "$(dirname "$BIN")" && pwd)
START='# >>> dropin-miner >>>'
END='# <<< dropin-miner <<<'
ENV_LINES=$(
  printf 'case ":$PATH:" in *":%s:"*) ;; *) export PATH="$PATH:%s" ;; esac\n' "$BIN_DIR" "$BIN_DIR"
  printf 'export TOKENDROP_CONFIG=%s\n' "$CFG"
  if [ -f "$WALLET_DIR/wallet.key" ]; then
    printf 'export TOKENDROP_WALLET_DIR=%s\n' "$WALLET_DIR"
  fi
)
CANDIDATE_PROFILE=""
case "${SHELL:-}" in
  *zsh)  CANDIDATE_PROFILE="$HOME/.zshrc" ;;
  *bash) CANDIDATE_PROFILE="$HOME/.bashrc" ;;
  *)     [ -f "$HOME/.bashrc" ] && CANDIDATE_PROFILE="$HOME/.bashrc" ;;
esac
say "Shell environment"
cat <<MSG
Your agents need TOKENDROP_API_KEY (your sr-… key from platform.nyks.dev) in
the shell they run from. Setup never writes that key anywhere. These lines
make the other commands short:

$(printf '%s\n' "$ENV_LINES" | sed 's/^/    /')
MSG
PROFILE=""; ANSWER=n
if [ -z "$CANDIDATE_PROFILE" ]; then
  echo "No ~/.bashrc or ~/.zshrc to add them to."
elif [ ! -t 0 ]; then
  echo "Not an interactive shell — not touching $CANDIDATE_PROFILE."
else
  printf '\nAdd them to %s? [Y/n]: ' "$CANDIDATE_PROFILE"
  read -r ANSWER || ANSWER=n
fi
case "$ANSWER" in
  ""|y|Y|yes|YES)
    if [ -n "$CANDIDATE_PROFILE" ]; then
      PROFILE="$CANDIDATE_PROFILE"
      if [ -f "$PROFILE" ] && grep -qF "$START" "$PROFILE"; then
        awk -v s="$START" -v e="$END" 'index($0,s){skip=1} !skip{print} index($0,e){skip=0}' "$PROFILE" > "$PROFILE.dropin-tmp" && mv "$PROFILE.dropin-tmp" "$PROFILE"
      fi
      { printf '%s\n' "$START"; printf '# Written by dropin-miner setup. Delete this block to undo.\n'; printf '%s\n' "$ENV_LINES"; printf '%s\n' "$END"; } >> "$PROFILE"
      say "Added a dropin-miner block to $PROFILE"
      printf '  open a new shell, or: source %s\n' "$PROFILE"
    fi ;;
  *) say "Left your shell profile alone" ;;
esac

# ── 8. coding agents ─────────────────────────────────────────────────────────
say "Coding agents"
if [ ! -t 0 ]; then
  echo "Not an interactive shell — not touching any agent. When you are ready:"
  printf '\n    %s agents install -config %s\n' "$BIN" "$CFG"
else
  cat <<MSG
Claude Code, Codex, Cursor and opencode can each get a web-search skill that
runs through the router, so their searches earn rewards. This writes a skill
file and, where the agent supports them, hook entries into its own config
(shown before anything is written).

MSG
  printf 'Set up the coding agents found on this machine now? [Y/n]: '
  read -r AGENTS || AGENTS=n
  case "$AGENTS" in
    ""|y|Y|yes|YES) "$BIN" agents install -yes -config "$CFG" || echo "Some agent could not be set up; see above." ;;
    *) echo "Left the agents alone. When you change your mind: $BIN agents install -config $CFG" ;;
  esac
fi

# ── 9. a first flush ─────────────────────────────────────────────────────────
say "First flush"
"$BIN" flush -config "$CFG" || true

if [ -n "$PROFILE" ]; then CMD="dropin-miner "; CFG_HINT=""; else CMD="$BIN "; CFG_HINT=" -config $CFG"; fi
cat <<MSG
──────────────────────────────────────────────────────────────────────────────
Setup complete.

1. Export your key in the shell your agents run from (setup did not):
     export TOKENDROP_API_KEY=sr-…      # from platform.nyks.dev → Keys
   Use a key from the SAME account you enrolled with; another account's key
   returns a clean 200 and earns nothing.

2. Your payout address: $PAYOUT
   It is in force NOW — "${CMD}payout show$CFG_HINT" says ACTIVE. Changing it
   later needs a Slot operator to approve; you keep being paid at the old
   address until they do.

3. Restart any coding agent that is already open, then search as usual. To
   try it by hand:
     ${CMD}search$CFG_HINT -format model "what is proof of authority consensus"
   Then: ${CMD}flush$CFG_HINT  and  ${CMD}doctor$CFG_HINT

Notes
  * Nothing runs between searches. Each search records itself and starts a
    short flush that joins the open epoch and submits; the session hooks do
    the same when an agent starts and stops.
  * First reward takes 1-2 hours — you join an epoch two ahead. One verified
    search per epoch makes you eligible; the pot splits equally.
──────────────────────────────────────────────────────────────────────────────
MSG
