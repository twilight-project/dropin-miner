#!/bin/sh
# dropin-miner setup — everything after the binary, asked as it goes.
#
#   Write the config, then hand off to `connect`: it registers with the
#   search platform, stores the key it mints, asks the mining question at
#   whichever terminal is present, creates or takes a wallet, and — once
#   you claim the printed URL — enrolls and declares a payout unattended.
#   No enrollment token to generate, no key to paste, no join to run by
#   hand. No service: the miner runs inside your agents' tool calls and
#   nowhere else.
#
# Env knobs (all optional):
#   TOKENDROP_BIN                     the dropin-miner binary (default:
#                                      ./bin/dropin-miner, then PATH)
#   TOKENDROP_HOME                    state directory (default ~/.tokendrop —
#                                      shared with a proxy if you run one)
#   TOKENDROP_SLOT / TOKENDROP_CHAIN / TOKENDROP_AS_URL / TOKENDROP_ROUTER_URL
#                                      the Slot to mine for (defaults: 3,
#                                      twilight-testnet-1, https://rewards.nyks.dev,
#                                      https://router-api.nyks.dev)
#   TOKENDROP_PLATFORM_URL / TOKENDROP_AGENTS_API_URL
#                                      the search platform (defaults:
#                                      https://platform.nyks.dev,
#                                      https://agents-v1.nyks.dev — two hosts,
#                                      not a typo; see README)
#   TOKENDROP_MINING=1                no terminal (piped, CI): answer the
#                                      mining question "yes" the way a
#                                      terminal would, matching connect's own
#                                      scripted-install path. Ignored when a
#                                      terminal IS present — that answer
#                                      always comes from the terminal.
#                                      TOKENDROP_PAYOUT_ADDRESS, if also set,
#                                      is used as the payout address.
set -eu

SLOT="${TOKENDROP_SLOT:-3}"
# Current target: the public testnet (twilight-testnet-1). Change these six
# lines (here + install.ps1 + README.md + npm/README.md) at the mainnet cutover.
CHAIN="${TOKENDROP_CHAIN:-twilight-testnet-1}"
AS_URL="${TOKENDROP_AS_URL:-https://rewards.nyks.dev}"
ROUTER="${TOKENDROP_ROUTER_URL:-https://router-api.nyks.dev}"
PLATFORM_URL="${TOKENDROP_PLATFORM_URL:-https://platform.nyks.dev}"
AGENTS_API_URL="${TOKENDROP_AGENTS_API_URL:-https://agents-v1.nyks.dev}"
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

# ── 1b. a previous installation ──────────────────────────────────────────────
# Nothing we ship ever deletes the state directory: it holds the wallet, the
# registration and the stored key. A user who removed the miner and comes
# back, or who set the directory aside as ~/.tokendrop.bak-*, should get
# those back rather than a second wallet and a second registration.
#
# What counts as an installation is any of: wallet/wallet.key,
# state/refresh.token (enrolled at the AS), state/agent.json (registered
# with the search platform — connect's own record, which may exist without
# the AS enrollment ever having completed), credentials.json. describe_install
# prints what a directory holds; adopt_install moves those pieces (and any
# unsent spool) into HOME_DIR, never overwriting one that is already there.
has_install(){ [ -f "$1/wallet/wallet.key" ] || [ -f "$1/state/refresh.token" ] || [ -f "$1/state/agent.json" ] || [ -f "$1/credentials.json" ]; }
describe_install(){
  d="$1"; parts=""
  if [ -f "$d/wallet/wallet.key" ]; then
    addr=$("$BIN" wallet address -dir "$d/wallet" 2>/dev/null || echo "?")
    parts="wallet $addr"
  fi
  if [ -f "$d/state/refresh.token" ] || [ -f "$d/state/agent.json" ]; then
    parts="${parts:+$parts, }enrolled"
  fi
  [ -f "$d/credentials.json" ] && parts="${parts:+$parts, }stored API key"
  [ -d "$d/spool" ] && [ -n "$(ls -A "$d/spool" 2>/dev/null)" ] && parts="${parts:+$parts, }unsent spool"
  printf '%s' "$parts"
}
adopt_install(){
  src="$1"
  mkdir -p "$HOME_DIR"
  for piece in wallet state credentials.json spool; do
    [ -e "$src/$piece" ] || continue
    if [ ! -e "$HOME_DIR/$piece" ]; then
      mv "$src/$piece" "$HOME_DIR/$piece"
      echo "  adopted $piece"
    elif [ -d "$src/$piece" ] && [ -d "$HOME_DIR/$piece" ]; then
      # The installer makes empty state/ and spool/ before setup runs, so a
      # directory already being there means nothing: merge, file by file,
      # keeping any file the destination already has. One exception: an
      # enrollment is the refresh token AND the DPoP key it is bound to,
      # together. A state/ with a key but no token is an enrollment that
      # never finished (a run that stopped at the token prompt made the key
      # first); its key would shadow the real one and every request would
      # fail "DPoP proof key does not match". Set it aside instead.
      if [ "$piece" = state ] && [ -f "$src/state/refresh.token" ] && [ ! -f "$HOME_DIR/state/refresh.token" ] && [ -f "$HOME_DIR/state/dpop.key" ]; then
        mv "$HOME_DIR/state/dpop.key" "$HOME_DIR/state/dpop.key.unenrolled-$(date +%Y%m%d%H%M%S)"
        echo "  set aside a DPoP key from an unfinished enrollment; the enrolled one is used"
      fi
      n=0; kept=0
      for f in "$src/$piece"/* "$src/$piece"/.[!.]*; do
        [ -e "$f" ] || continue
        b=$(basename "$f")
        if [ -e "$HOME_DIR/$piece/$b" ]; then kept=$((kept+1)); continue; fi
        mv "$f" "$HOME_DIR/$piece/$b"; n=$((n+1))
      done
      rmdir "$src/$piece" 2>/dev/null || true
      echo "  adopted $piece ($n file(s)$([ $kept -gt 0 ] && echo ", $kept already present kept"))"
    else
      echo "  keeping the $piece already in $HOME_DIR (not overwritten by $src/$piece)"
    fi
  done
}

if has_install "$HOME_DIR"; then
  say "Previous installation found in $HOME_DIR: $(describe_install "$HOME_DIR")"
  echo "Its wallet, registration and key are used as they are; only what is missing is set up."
else
  # Siblings a person or an earlier removal might have left: ~/.tokendrop.bak-DATE,
  # ~/.tokendrop.old, ~/.tokendrop-anything. Newest first.
  FOUND=""
  for cand in $(ls -dt "$HOME_DIR".* "$HOME_DIR"-* 2>/dev/null); do
    [ -d "$cand" ] || continue
    has_install "$cand" || continue
    FOUND="$cand"; break
  done
  if [ -n "$FOUND" ]; then
    say "A previous installation is set aside at $FOUND"
    echo "It holds: $(describe_install "$FOUND")"
    echo "Using it means the same wallet, the same registration and no new tokens to generate."
    if [ -t 0 ]; then
      printf '\nUse it? [Y/n]: '
      read -r ADOPT || ADOPT=n
    else
      echo "Not an interactive shell — not touching it. Move it to $HOME_DIR yourself to reuse it."
      ADOPT=n
    fi
    case "$ADOPT" in
      ""|y|Y|yes|YES)
        adopt_install "$FOUND"
        if [ -z "$(ls -A "$FOUND" 2>/dev/null)" ]; then rmdir "$FOUND" && echo "  removed the now-empty $FOUND"
        else echo "  left the rest of $FOUND in place (config, logs); delete it when you like"; fi
        ;;
      *) echo "Left it alone. A fresh wallet and registration follow." ;;
    esac
  fi
fi

# A stored mining decision can travel silently with an adopted state/
# directory (it is just one more file in the merge above); say what it is
# rather than let connect's silence on the question read as "it forgot to
# ask" — connect will not ask again while a decision is already on file.
if [ -f "$HOME_DIR/state/mining_decision.json" ]; then
  if grep -q '"enabled":true' "$HOME_DIR/state/mining_decision.json" 2>/dev/null; then
    say "A stored mining decision came with this installation: mining is ON. connect will not ask again."
  else
    say "A stored mining decision came with this installation: mining is OFF."
    echo "To turn it on: $BIN mining enable -config $HOME_DIR/tokendrop.toml"
  fi
fi

# ── 2. directories (0700 matters: keys and spooled records live here) ────────
mkdir -p "$HOME_DIR/state" "$HOME_DIR/spool" "$HOME_DIR/intake" "$HOME_DIR/sessions"
chmod 700 "$HOME_DIR" "$HOME_DIR/state" "$HOME_DIR/spool" "$HOME_DIR/intake" "$HOME_DIR/sessions"

# ── 3. config ────────────────────────────────────────────────────────────────
CFG="$HOME_DIR/tokendrop.toml"
if [ -f "$CFG" ] && grep -q '^\[miner\]' "$CFG"; then
  say "Config already has a [miner] block: $CFG (left as is)"
else
  # [mining] never says enabled = true unconditionally: a terminal answers
  # connect's own question, and that answer is the decision. Only a
  # genuinely non-interactive run (no terminal, TOKENDROP_MINING=1) writes
  # it here — connect persists whatever this ends up saying either way, so
  # every install ends with a decision on file, never an absent one.
  #
  # Two heredocs with a plain printf between them, not a spliced variable:
  # command substitution strips trailing newlines, and a MINING_LINES
  # variable built by concatenating two of them lost the newline between
  # "enabled = true" and "payout_address = ...", writing invalid TOML
  # ("truepayout_address"). Appending directly to the file has no such
  # trap.
  cat > "$CFG" <<TOML
[[provider]]
name     = "search-router"
upstream = "$ROUTER"   # the GATEWAY, not the verification API

[platform]
base_url       = "$PLATFORM_URL"
agents_api_url = "$AGENTS_API_URL"

[mining]
TOML
  if [ ! -t 0 ] && [ "${TOKENDROP_MINING:-}" = "1" ]; then
    printf 'enabled   = true\n' >> "$CFG"
    if [ -n "${TOKENDROP_PAYOUT_ADDRESS:-}" ]; then
      printf 'payout_address = "%s"\n' "$TOKENDROP_PAYOUT_ADDRESS" >> "$CFG"
    fi
  fi
  cat >> "$CFG" <<TOML
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

# ── 4. connect ────────────────────────────────────────────────────────────────
# Registers with the search platform (storing the key it mints — nothing to
# paste), asks the mining question at a terminal when one is present (the
# config above already answered it otherwise), and creates or takes a wallet.
# Prints the claim URL and waits a few minutes for it; if nobody has claimed
# by then it says so and exits 0 — the claim still works whenever it happens,
# picked up automatically by your first real search. TOKENDROP_WALLET_DIR is
# exported so a wallet connect creates lands beside the rest of this
# installation, not the default OS config directory.
say "Connecting"
WALLET_DIR="$HOME_DIR/wallet"
export TOKENDROP_WALLET_DIR="$WALLET_DIR"
"$BIN" connect -mining -config "$CFG" || die "connect failed"

# ── 5. shell profile ─────────────────────────────────────────────────────────
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
These lines make the other commands short. Your key is not among them: a
search reads it from the stored credentials file (or TOKENDROP_API_KEY, if a
shell exports one, which then wins):

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

# ── 6. coding agents ─────────────────────────────────────────────────────────
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

if [ -n "$PROFILE" ]; then CMD="dropin-miner "; CFG_HINT=""; else CMD="$BIN "; CFG_HINT=" -config $CFG"; fi
cat <<MSG
──────────────────────────────────────────────────────────────────────────────
Setup complete.

1. If a claim URL was printed above and nobody has visited it yet, do that
   whenever you're ready — nothing else here depends on timing it. Once
   claimed, mining (if you said yes) enrolls and declares a payout on its
   own, from the next search.

2. Restart any coding agent that is already open, then search as usual. To
   try it by hand:
     ${CMD}search$CFG_HINT -format model "what is proof of authority consensus"

3. Check in any time:
     ${CMD}status$CFG_HINT   ${CMD}doctor$CFG_HINT   ${CMD}payout show$CFG_HINT

Notes
  * Nothing runs between searches. Each search records itself and starts a
    short flush that joins the open epoch and submits; the session hooks do
    the same when an agent starts and stops.
  * First reward takes 1-2 hours — you join an epoch two ahead. One verified
    search per epoch makes you eligible; the pot splits equally.
──────────────────────────────────────────────────────────────────────────────
MSG
