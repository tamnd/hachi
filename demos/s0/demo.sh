#!/usr/bin/env bash
# The S0 gate you can watch: one real task end-to-end in a real repo.
# hachi opens in a fresh git repo, codex reads a file and answers, and
# the journal keeps the evidence. Four phases: setup, run, assert,
# archive.
#
# Exit 0 pass, 1 a named check failed, 3 infra (expect or codex missing,
# or codex logged out), so an upstream wobble never reads as a hachi
# regression.
#
# Knobs: HACHI_BIN to run a prebuilt binary instead of building one,
# DEMO_OUT to choose the artifact directory.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"

say() { printf '%s\n' "$*"; }

# --- setup: pin everything pinnable -----------------------------------

command -v expect >/dev/null || { say "infra: expect not installed"; exit 3; }
command -v codex >/dev/null || { say "infra: codex not on PATH"; exit 3; }
codex login status >/dev/null 2>&1 || { say "infra: codex not logged in"; exit 3; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

hachi_bin="${HACHI_BIN:-$work/hachi}"
if [ ! -x "$hachi_bin" ]; then
  say "building hachi from $repo_root"
  (cd "$repo_root" && go build -o "$hachi_bin" ./cmd/hachi)
fi

hive="$work/hive"   # throwaway HACHI_HOME; the real journal stays untouched
target="$work/repo" # the real repo the task runs in
mkdir -p "$hive" "$target"

cat >"$target/main.go" <<'EOF'
package main

import "fmt"

func main() {
	fmt.Println("WAGGLE")
}
EOF
git -C "$target" init -q
git -C "$target" add main.go
git -C "$target" -c user.name=demo -c user.email=demo@demo.invalid commit -qm "seed"

# --- run: a real codex turn through a real pty -------------------------

transcript="$work/transcript.log"
export HACHI_HOME="$hive"
export DEMO_BIN="$hachi_bin" DEMO_DIR="$target" DEMO_LOG="$transcript"
export DEMO_PROMPT="Open main.go and tell me the exact word this program prints. Answer with just that word."

set +e
expect <<'EXPECT'
proc ok {name} { send_user "\ncheck ok: $name\n" }
proc fail {name} { send_user "\ncheck fail: $name\n"; exit 1 }

log_file -noappend $env(DEMO_LOG)
cd $env(DEMO_DIR)
spawn $env(DEMO_BIN)
# expect's pty starts sizeless and the TUI waits for a size before its
# first render; give it a real screen.
exec stty rows 42 columns 160 < $spawn_out(slave,name)

# Match banner text, not the composer placeholder: the blinking cursor
# splits the placeholder with style escapes in the raw stream.
set timeout 30
expect {
    "the glass hive" { ok "welcome screen" }
    timeout { fail "welcome screen never rendered" }
}
send -- $env(DEMO_PROMPT)
sleep 0.3
send "\r"

set timeout 180
expect {
    "WAGGLE" { ok "the answer showed up in the transcript" }
    timeout { fail "no answer within three minutes" }
}
set timeout 60
expect {
    "idle" { ok "the turn wound down to idle" }
    timeout { fail "turn never wound down" }
}
send "\003"
expect eof
EXPECT
run_status=$?
set -e

# --- archive first: the journal is the evidence, pass or fail ----------

out="${DEMO_OUT:-$here/artifacts/$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$out"
cp -R "$hive/sessions" "$out/journal" 2>/dev/null || true
cp "$transcript" "$out/transcript.log" 2>/dev/null || true
{
  "$hachi_bin" --version
  git -C "$repo_root" rev-parse HEAD 2>/dev/null || say "hachi checkout: not a git checkout"
  codex --version
} >"$out/pins.txt" 2>&1
say "artifacts: $out"

[ "$run_status" -eq 0 ] || { say "gate failed in the pty run; read $out/transcript.log"; exit 1; }

# --- assert: the journal must hold what the screen showed --------------

events="$(find "$hive/sessions" -name events.jsonl 2>/dev/null | head -1)"
[ -n "$events" ] || { say "check fail: no events.jsonl in the throwaway hive"; exit 1; }
say "check ok: journal exists ($events)"

grep -q '"bee":"human","kind":"message"' "$events" ||
  { say "check fail: the human message never reached the journal"; exit 1; }
say "check ok: the prompt is journaled"

grep '"bee":"codex","kind":"message"' "$events" | grep -q WAGGLE ||
  { say "check fail: no codex message with the answer in the journal"; exit 1; }
say "check ok: the answer is journaled"

grep -q '"resume":' "$(dirname "$events")/meta.json" ||
  { say "check fail: no resume handle saved; reopen would start a fresh thread"; exit 1; }
say "check ok: resume handle saved"

say "PASS: hachi held a real codex conversation in a real repo, and the journal proves it"
