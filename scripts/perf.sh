#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Synthetic fleet benchmark against the budgets in docs/REQUIREMENTS.md (NFR-1, NFR-3).
#   N=500 ./scripts/perf.sh        (needs: git, curl, the binary in ./bin or REPO_KEEPER=...)
# Everything lives in a temp dir; HOME, XDG_* and git config are isolated.
set -euo pipefail

N="${N:-500}"
IDLE_SECONDS="${IDLE_SECONDS:-30}"
BIN="${REPO_KEEPER:-$(cd "$(dirname "$0")/.." && pwd)/bin/repo-keeper}"
BUDGET_RSS_MB="${BUDGET_RSS_MB:-50}"
BUDGET_IDLE_CPU="${BUDGET_IDLE_CPU:-0.5}"      # percent of one core
BUDGET_START_MS="${BUDGET_START_MS:-300}"
BUDGET_UI_MS="${BUDGET_UI_MS:-500}"

T=$(mktemp -d)
export HOME="$T/home" XDG_CONFIG_HOME="$T/cfg" XDG_STATE_HOME="$T/state" XDG_RUNTIME_DIR="$T/run"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=perf GIT_AUTHOR_EMAIL=perf@example.invalid GIT_COMMITTER_NAME=perf GIT_COMMITTER_EMAIL=perf@example.invalid
mkdir -p "$HOME" "$XDG_RUNTIME_DIR" "$T/origins" "$T/code"; chmod 700 "$XDG_RUNTIME_DIR"
PID=""
cleanup() { [ -n "$PID" ] && kill "$PID" 2>/dev/null || true; rm -rf "$T"; }
trap cleanup EXIT

now_ms() { date +%s%3N; }
rss_kb()  { awk '/^VmRSS/ {print $2}' "/proc/$PID/status"; }
hwm_kb()  { awk '/^VmHWM/ {print $2}' "/proc/$PID/status"; }
cpu_ticks() { awk '{print $14+$15}' "/proc/$PID/stat"; }
HZ=$(getconf CLK_TCK)

echo "== building a fleet of $N repositories"
t0=$(now_ms)
git init -q --bare -b main "$T/seed.git"
git clone -q "$T/seed.git" "$T/seed" 2>/dev/null
( cd "$T/seed" && git checkout -q -b main && for i in 1 2 3 4 5; do echo "line $i" >> README.md; git add README.md; git commit -q -m "c$i"; done && git push -q -u origin main )
for i in $(seq 1 "$N"); do
  git clone -q --bare "$T/seed.git" "$T/origins/r$i.git"
  git clone -q "$T/origins/r$i.git" "$T/code/r$i" 2>/dev/null
done
echo "   built in $(( $(now_ms) - t0 )) ms"

mkdir -p "$XDG_CONFIG_HOME/repo-keeper"
{
  printf '[general]\ninterval = "30m"\nconcurrency = 4\n[cleanup]\nmode = "dry-run"\n[ui]\nenabled = true\n'
  for i in $(seq 1 "$N"); do printf '[[repo]]\npath = "%s/code/r%s"\n' "$T" "$i"; done
} > "$XDG_CONFIG_HOME/repo-keeper/config.toml"

echo "== starting the daemon"
t_start=$(now_ms)
"$BIN" daemon --log-level warn >"$T/daemon.log" 2>&1 &
PID=$!
while [ ! -f "$XDG_RUNTIME_DIR/repo-keeper/ui.json" ]; do
  kill -0 "$PID" 2>/dev/null || { echo "daemon died:"; cat "$T/daemon.log"; exit 1; }
  sleep 0.01
done
START_MS=$(( $(now_ms) - t_start ))

echo "== first full cycle"
t_cycle=$(now_ms); peak=0
done_count() { "$BIN" status --json 2>/dev/null | grep -c '"Status": "ok"' || true; }
while :; do
  c=$(done_count)
  r=$(hwm_kb); [ "$r" -gt "$peak" ] && peak=$r
  [ "$c" -ge "$N" ] && break
  [ $(( $(now_ms) - t_cycle )) -gt 600000 ] && { echo "first cycle did not finish in 10 min ($c/$N)"; exit 1; }
  sleep 0.5
done
CYCLE_MS=$(( $(now_ms) - t_cycle ))
PEAK_MB=$(( $(hwm_kb) / 1024 ))

echo "== idle for ${IDLE_SECONDS}s (every repo's next sync is ~30 min away)"
sleep 3
c0=$(cpu_ticks); w0=$(now_ms)
sleep "$IDLE_SECONDS"
c1=$(cpu_ticks); w1=$(now_ms)
IDLE_CPU=$(awk -v t=$((c1-c0)) -v hz="$HZ" -v ms=$((w1-w0)) 'BEGIN{printf "%.3f", (t/hz)/(ms/1000)*100}')
IDLE_MB=$(( $(rss_kb) / 1024 ))

echo "== UI with $N repositories"
URL=$("$BIN" ui --print-url)
JAR="$T/jar"; ADDR=$(echo "$URL" | sed 's#http://\([^/]*\)/.*#\1#')
curl -s -c "$JAR" -o /dev/null "$URL"
UI_MS=$(curl -s -b "$JAR" -o /dev/null -w '%{time_total}' "http://$ADDR/" | awk '{printf "%d", $1*1000}')
FRAG_MS=$(curl -s -b "$JAR" -o /dev/null -w '%{time_total}' "http://$ADDR/fragments/dashboard" | awk '{printf "%d", $1*1000}')
PAGE_KB=$(curl -s -b "$JAR" "http://$ADDR/" | wc -c | awk '{printf "%d", $1/1024}')
DB_KB=$(du -k "$XDG_STATE_HOME/repo-keeper/state.db" | awk '{print $1}')

printf '\n%-34s %10s %12s\n' "metric" "measured" "budget"
printf '%-34s %10s %12s\n' "startup to UI listening (ms)" "$START_MS" "< $BUDGET_START_MS"
printf '%-34s %10s %12s\n' "first cycle, $N repos (s)" "$(( CYCLE_MS / 1000 ))" "-"
printf '%-34s %10s %12s\n' "peak RSS during cycle (MB)" "$PEAK_MB" "< $BUDGET_RSS_MB"
printf '%-34s %10s %12s\n' "idle RSS (MB)" "$IDLE_MB" "< $BUDGET_RSS_MB"
printf '%-34s %10s %12s\n' "idle CPU (% of one core)" "$IDLE_CPU" "< $BUDGET_IDLE_CPU"
printf '%-34s %10s %12s\n' "dashboard response (ms)" "$UI_MS" "< $BUDGET_UI_MS"
printf '%-34s %10s %12s\n' "dashboard fragment (ms)" "$FRAG_MS" "-"
printf '%-34s %10s %12s\n' "dashboard HTML size (KB)" "$PAGE_KB" "-"
printf '%-34s %10s %12s\n' "state.db size (KB)" "$DB_KB" "-"

fail=0
[ "$START_MS" -lt "$BUDGET_START_MS" ] || { echo "FAIL startup"; fail=1; }
[ "$PEAK_MB" -lt "$BUDGET_RSS_MB" ] || { echo "FAIL peak RSS"; fail=1; }
awk -v v="$IDLE_CPU" -v b="$BUDGET_IDLE_CPU" 'BEGIN{exit !(v<b)}' || { echo "FAIL idle CPU"; fail=1; }
[ "$UI_MS" -lt "$BUDGET_UI_MS" ] || { echo "FAIL UI latency"; fail=1; }
[ "$fail" = 0 ] && echo "all budgets met" || exit 1
