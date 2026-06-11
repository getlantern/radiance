#!/usr/bin/env bash
#
# memprofile.sh — drive lanternd through memory-stressing scenarios while
# capturing pprof profiles and an RSS/throughput time series, so heap growth
# can be attributed and separated from working set vs. leak.
#
# Requires: a lanternd built with --pprof-addr enabled and currently running
# with the endpoint reachable (default 127.0.0.1:6060), plus the `lantern` CLI,
# `curl`, `jq`, `ps`, and `pgrep` on PATH. Runs as an unprivileged user: process
# RSS and the loopback pprof endpoint are both readable without root.
#
# Linux and macOS. On Linux, RSS/peak-RSS/threads are read from /proc/<pid>/status
# and the load fleet runs in a setsid process group for atomic teardown. On macOS
# they come from `ps`; macOS has no live peak-RSS (VmHWM) counter, so hwm_kb is
# approximated by the running max of sampled RSS, and the fleet is reaped by
# walking its process tree (no setsid).
#
# Usage:
#   scripts/memprofile.sh [-cpu] [-mem] [phase ...]
# -mem  capture heap/goroutine/allocs snapshots per phase (the original behavior).
# -cpu  capture a CPU profile spanning each sustained-work window (heavy, idle);
#       with no explicit phases, runs the minimal set bracketing those windows:
#       connect heavy idle end.
# With neither flag, both are collected. Explicit phases override the per-mode default.
# Phases: baseline connect heavy drain idle toggle end
#
# Tunables (env vars, with defaults):
#   PPROF_ADDR=127.0.0.1:6060   LOAD_URL=https://speed.cloudflare.com/__down?bytes=104857600
#   MON_INTERVAL=2   CONNECT_WARMUP=20   SETTLE=10
#   LOAD_DURATION=120   LOAD_CONCURRENCY=20   DRAIN_SETTLE=30   DRAIN_CONNS=2
#   IDLE_DURATION=600   TOGGLE_CYCLES=5   TOGGLE_SETTLE=10
#   OUTDIR=./memprofile-<timestamp>

set -u

PPROF_ADDR="${PPROF_ADDR:-127.0.0.1:6060}"
LOAD_URL="${LOAD_URL:-https://speed.cloudflare.com/__down?bytes=104857600}"
MON_INTERVAL="${MON_INTERVAL:-2}"
CONNECT_WARMUP="${CONNECT_WARMUP:-20}"
SETTLE="${SETTLE:-10}"
LOAD_DURATION="${LOAD_DURATION:-120}"
LOAD_CONCURRENCY="${LOAD_CONCURRENCY:-20}"
DRAIN_SETTLE="${DRAIN_SETTLE:-30}"
DRAIN_CONNS="${DRAIN_CONNS:-2}"
IDLE_DURATION="${IDLE_DURATION:-600}"
TOGGLE_CYCLES="${TOGGLE_CYCLES:-5}"
TOGGLE_SETTLE="${TOGGLE_SETTLE:-10}"
OUTDIR="${OUTDIR:-./memprofile-$(date +%Y%m%d-%H%M%S)}"

OS="$(uname -s)"
HAVE_SETSID=0
command -v setsid >/dev/null 2>&1 && HAVE_SETSID=1

PHASE_FILE=""
SAMPLES_CSV=""
CAPTURES_CSV=""
SAMPLER_PID=""    # handle for the background sampler (group leader or tree root)
LOAD_PID=""       # handle for the load-generator fleet (group leader or tree root)
_GROUP_PID=""     # set by spawn_group to the latest background unit's handle
_CLEANED=0
DO_CPU=0          # -cpu; defaults to both when neither flag is given
DO_MEM=0          # -mem
CPU_PIDS=()       # in-flight CPU-profile captures, awaited between phases

log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" >&2; }

# worker_pid prints the lanternd process actually running the daemon: the leaf
# of the babysit chain (a lanternd pid that is not the parent of another
# lanternd). A babysit restart can briefly expose two leaves (old + new child),
# so among leaves we pick the one with the most threads — the worker runs the
# full backend (~20+ threads) while a transient/babysit leaf has far fewer.
# Re-resolved on demand so sampling survives a restart.
worker_pid() {
  local pids p q is_parent best best_thr thr
  pids=$(pgrep -x lanternd 2>/dev/null) || return 1
  best=""; best_thr=-1
  for p in $pids; do
    is_parent=0
    for q in $pids; do
      [ "$q" = "$p" ] && continue
      [ "$(ppid_of "$q")" = "$p" ] && { is_parent=1; break; }
    done
    [ "$is_parent" -eq 0 ] || continue
    thr=$(thread_count "$p"); [ -z "$thr" ] && thr=0
    if [ "$thr" -gt "$best_thr" ]; then best=$p; best_thr=$thr; fi
  done
  [ -n "$best" ] && { echo "$best"; return 0; }
  return 1
}

# ppid_of, thread_count, rss_of, hwm_of read per-process stats portably: from
# /proc on Linux, from ps on macOS (no /proc). Each prints empty when unavailable.
ppid_of() {
  if [ "$OS" = Darwin ]; then ps -o ppid= -p "$1" 2>/dev/null | tr -d '[:space:]'
  else awk '/^PPid:/{print $2}' "/proc/$1/status" 2>/dev/null; fi
}
thread_count() {
  if [ "$OS" = Darwin ]; then
    local n; n=$(ps -M -p "$1" 2>/dev/null | tail -n +2 | grep -c .)
    [ "${n:-0}" -gt 0 ] && echo "$n"
  else awk '/^Threads:/{print $2}' "/proc/$1/status" 2>/dev/null; fi
}
rss_of() { # kB
  if [ "$OS" = Darwin ]; then ps -o rss= -p "$1" 2>/dev/null | tr -d '[:space:]'
  else awk '/^VmRSS:/{print $2}' "/proc/$1/status" 2>/dev/null; fi
}
hwm_of() { # kB; empty on macOS, which has no live peak-RSS counter
  [ "$OS" = Darwin ] && return 0
  awk '/^VmHWM:/{print $2}' "/proc/$1/status" 2>/dev/null
}

# peak_rss tracks the running max of rss across calls via a file in OUTDIR, so the
# sampler subshell and checkpoint captures share one high-water mark on macOS,
# which cannot read VmHWM. Non-numeric input is passed through unchanged.
peak_rss() { # rss_kb
  local cur=$1 f prev tmp
  case "$cur" in '' | *[!0-9]*) echo "$cur"; return ;; esac
  f="$OUTDIR/.peakrss"
  prev=$(cat "$f" 2>/dev/null); [ -z "$prev" ] && prev=0
  [ "$cur" -gt "$prev" ] && prev=$cur
  # Rename atomically: the sampler subshell and checkpoint captures both update
  # this, so a reader must never catch a half-written file. A racing update can
  # still be lost, but only costs one sample of peak resolution.
  tmp="$f.$$.$RANDOM"
  echo "$prev" >"$tmp" && mv "$tmp" "$f"
  echo "$prev"
}

# sample_proc prints "rss hwm threads" for the current worker. Any field that
# can't be read — no resolvable worker, or the worker vanishing mid-read during
# a babysit restart — becomes NA so gaps stay distinct from a real 0.
sample_proc() {
  local wp rss="" hwm="" thr=""
  wp=$(worker_pid) || true
  if [ -n "$wp" ]; then
    rss=$(rss_of "$wp"); thr=$(thread_count "$wp")
    if [ "$OS" = Darwin ]; then hwm=$(peak_rss "$rss"); else hwm=$(hwm_of "$wp"); fi
  fi
  echo "${rss:-NA} ${hwm:-NA} ${thr:-NA}"
}

status_line() { lantern status 2>/dev/null | head -1; }

# one_snapshot prints "active<TAB>up<TAB>down". throughput is a one-shot
# (non-streaming) command, so there is no pipe to tear down and no streaming
# process that could be left running.
one_snapshot() {
  lantern throughput --json 2>/dev/null \
    | jq -r '[.active_connections, .global.up, .global.down]|@tsv' 2>/dev/null
}

# set_phase writes the tag atomically (temp + rename) so a sampler read can't
# observe the file mid-truncate and record a blank phase for that tick.
set_phase() { echo "$1" > "$PHASE_FILE.tmp" && mv "$PHASE_FILE.tmp" "$PHASE_FILE"; log "=== phase: $1 ==="; }

wait_status() { # target, timeout_s
  local target=$1 t=${2:-60} i=0
  while [ "$i" -lt "$t" ]; do
    [ "$(status_line)" = "$target" ] && return 0
    sleep 1; i=$((i+1))
  done
  log "WARN: timed out waiting for status=$target"; return 1
}

wait_conns_below() { # threshold, timeout_s
  local thr=$1 t=${2:-120} i=0 active
  while [ "$i" -lt "$t" ]; do
    active=$(one_snapshot | cut -f1)
    if [[ "$active" =~ ^[0-9]+$ ]] && [ "$active" -le "$thr" ]; then
      return 0
    fi
    sleep 2; i=$((i+2))
  done
  log "WARN: timed out waiting for active_connections<=$thr"; return 1
}

# grab_profile fetches one pprof endpoint to a temp file and renames it into
# place only on success, so an endpoint hiccup (daemon restart, 500) leaves no
# half-written .pprof masquerading as a valid capture.
grab_profile() { # url, dest, what
  if curl -fsS --max-time 30 "$1" -o "$2.tmp" 2>/dev/null && [ -s "$2.tmp" ]; then
    mv "$2.tmp" "$2"; return 0
  fi
  rm -f "$2.tmp"; log "WARN: capture failed: $3"; return 1
}

# grab_cpu starts a CPU profile spanning the next `secs` seconds in the background
# so it overlaps the phase's load/soak window instead of blocking it. Go permits
# only one CPU profile at a time, so callers must wait_cpu before the next phase
# starts one; phases are sequential, so awaiting at phase end suffices.
grab_cpu() { # seconds, label
  local secs=$1 L=$2 dest="$OUTDIR/cpu.$L.pprof"
  (
    if curl -fsS --max-time "$((secs + 30))" "http://$PPROF_ADDR/debug/pprof/profile?seconds=$secs" -o "$dest.tmp" 2>/dev/null && [ -s "$dest.tmp" ]; then
      mv "$dest.tmp" "$dest"
    else
      rm -f "$dest.tmp"; log "WARN: cpu capture failed: $L"
    fi
  ) &
  CPU_PIDS+=("$!")
  log "cpu profiling started: $L (${secs}s window)"
}

# wait_cpu blocks until in-flight CPU captures finish, so the next phase can't
# start a second (rejected) profile and an exit can't truncate the capture.
wait_cpu() {
  [ ${#CPU_PIDS[@]} -eq 0 ] && return 0
  wait "${CPU_PIDS[@]}" 2>/dev/null
  CPU_PIDS=()
}

capture() { # label
  local L=$1 ts rss hwm thr conns
  ts=$(date +%s)
  if [ "$DO_MEM" -eq 1 ]; then
    grab_profile "http://$PPROF_ADDR/debug/pprof/heap?gc=1" "$OUTDIR/heap.$L.pprof" "heap ($L)"
    grab_profile "http://$PPROF_ADDR/debug/pprof/goroutine" "$OUTDIR/goroutine.$L.pprof" "goroutine ($L)"
    grab_profile "http://$PPROF_ADDR/debug/pprof/allocs" "$OUTDIR/allocs.$L.pprof" "allocs ($L)"
  fi
  read -r rss hwm thr <<<"$(sample_proc)"
  conns=$(one_snapshot | cut -f1)
  echo "$ts,$L,$rss,$hwm,$thr,${conns:-NA}" >> "$CAPTURES_CSV"
  log "captured '$L': rss=${rss}kB hwm=${hwm}kB threads=${thr} conns=${conns:-?}"
}

# kill_tree kills $1 and all its descendants, parent before children so a
# respawning worker loop stops before we reap the curl it would otherwise
# immediately replace.
kill_tree() { # pid
  local pid=$1 child kids
  kids=$(pgrep -P "$pid" 2>/dev/null)
  kill "$pid" 2>/dev/null
  for child in $kids; do kill_tree "$child"; done
}

# spawn_group launches "$@" in the background as one killable unit and sets
# _GROUP_PID to its handle: a process-group leader under setsid (Linux), otherwise
# a plain background pid reaped via its process tree. The setsid path takes $! as
# the group leader, which holds only because this script runs without job control
# (no set -m) and is executed, not sourced.
spawn_group() {
  if [ "$HAVE_SETSID" -eq 1 ]; then setsid "$@" &
  else "$@" &
  fi
  _GROUP_PID=$!
}

# stop_group terminates a spawn_group unit: a group kill under setsid, else a
# process-tree walk.
stop_group() { # handle
  [ -n "$1" ] || return 0
  if [ "$HAVE_SETSID" -eq 1 ]; then kill -- "-$1" 2>/dev/null
  else kill_tree "$1"
  fi
}

# start_load runs the whole load-generator fleet as one killable unit (see
# spawn_group) so stop_load takes down the workers AND their in-flight curls
# together — a bare kill of the launcher leaves its current curl orphaned and
# still flooding the tunnel.
start_load() { # concurrency
  local conc=$1
  spawn_group bash -c '
    url=$1; n=$2
    for _ in $(seq 1 "$n"); do
      ( while :; do curl -s --max-time 30 -o /dev/null "$url" || true; done ) &
    done
    wait
  ' _ "$LOAD_URL" "$conc"
  LOAD_PID=$_GROUP_PID
  log "load started: $conc workers (pid $LOAD_PID) -> $LOAD_URL"
}

stop_load() {
  stop_group "$LOAD_PID"
  LOAD_PID=""
}

start_sampler() {
  spawn_group bash -c '
    outdir=$1; phase_file=$2; csv=$3; interval=$4
    # shellcheck source=/dev/null
    . "$outdir/.samplerlib"
    while :; do
      ts=$(date +%s)
      phase=$(cat "$phase_file" 2>/dev/null)
      read -r rss hwm thr <<<"$(sample_proc)"
      fields=$(one_snapshot)
      active=$(echo "$fields" | cut -f1); up=$(echo "$fields" | cut -f2); down=$(echo "$fields" | cut -f3)
      echo "$ts,${phase:-},$rss,$hwm,$thr,${active:-NA},${up:-NA},${down:-NA}" >> "$csv"
      sleep "$interval"
    done
  ' _ "$OUTDIR" "$PHASE_FILE" "$SAMPLES_CSV" "$MON_INTERVAL"
  SAMPLER_PID=$_GROUP_PID
  log "sampler started (pid $SAMPLER_PID)"
}

cleanup() {
  [ "$_CLEANED" = 1 ] && return
  _CLEANED=1
  log "cleaning up"
  stop_load
  # Each CPU_PIDS entry is the ( ... ) & subshell wrapping curl; kill_tree reaps
  # the curl too, so an interrupted profile doesn't keep the daemon's single
  # CPU-profile slot held open against the next run.
  if [ ${#CPU_PIDS[@]} -gt 0 ]; then
    local p; for p in "${CPU_PIDS[@]}"; do kill_tree "$p"; done
  fi
  stop_group "$SAMPLER_PID"
  SAMPLER_PID=""
}

phase_baseline() {
  set_phase baseline_disconnected
  if [ "$(status_line)" != "Disconnected" ]; then
    lantern disconnect >/dev/null 2>&1; wait_status Disconnected 30
  fi
  sleep "$SETTLE"
  capture baseline_disconnected
}

phase_connect() {
  set_phase connect
  lantern connect >/dev/null 2>&1; wait_status Connected 60
  log "connected; warmup ${CONNECT_WARMUP}s"
  sleep "$CONNECT_WARMUP"
  set_phase baseline_connected
  capture baseline_connected
}

phase_heavy() {
  [ "$(status_line)" = "Connected" ] || { log "skip heavy: not connected"; return; }
  set_phase heavy
  start_load "$LOAD_CONCURRENCY"
  [ "$DO_CPU" -eq 1 ] && grab_cpu "$LOAD_DURATION" heavy
  sleep "$LOAD_DURATION"
  capture heavy_peak   # captured while load is still running
  wait_cpu
  stop_load
}

phase_drain() {
  [ "$(status_line)" = "Connected" ] || { log "skip drain: not connected"; return; }
  set_phase drain
  stop_load
  wait_conns_below "$DRAIN_CONNS" 120
  sleep "$DRAIN_SETTLE"
  capture drain
}

phase_idle() {
  [ "$(status_line)" = "Connected" ] || { log "skip idle: not connected"; return; }
  set_phase idle
  log "idle soak ${IDLE_DURATION}s (ambient traffic still flows through the tunnel)"
  [ "$DO_CPU" -eq 1 ] && grab_cpu "$IDLE_DURATION" idle
  sleep "$IDLE_DURATION"
  capture idle_end
  wait_cpu
}

phase_toggle() {
  set_phase toggle
  local c
  for c in $(seq 1 "$TOGGLE_CYCLES"); do
    lantern disconnect >/dev/null 2>&1; wait_status Disconnected 30
    lantern connect >/dev/null 2>&1; wait_status Connected 60
    log "toggle cycle $c/$TOGGLE_CYCLES done"
  done
  sleep "$TOGGLE_SETTLE"
  capture toggle
}

phase_end() {
  set_phase end
  lantern disconnect >/dev/null 2>&1; wait_status Disconnected 30
  sleep "$SETTLE"
  capture end_disconnected
}

# write_samplerlib exports the helpers the sampler subshell needs, since it runs
# under a fresh `bash -c` that does not inherit this script's function definitions.
write_samplerlib() {
  {
    declare -f worker_pid ppid_of thread_count rss_of hwm_of peak_rss sample_proc one_snapshot
    echo "OS='$OS'"
    echo "OUTDIR='$OUTDIR'"
    echo "PPROF_ADDR='$PPROF_ADDR'"
  } > "$OUTDIR/.samplerlib"
}

main() {
  local tool
  for tool in lantern curl jq ps pgrep; do
    command -v "$tool" >/dev/null || { echo "$tool not on PATH" >&2; exit 1; }
  done
  curl -sf --max-time 5 "http://$PPROF_ADDR/debug/pprof/" -o /dev/null \
    || { echo "pprof endpoint $PPROF_ADDR unreachable; start lanternd with --pprof-addr" >&2; exit 1; }
  worker_pid >/dev/null || { echo "no running lanternd worker found" >&2; exit 1; }

  local args=()
  while [ $# -gt 0 ]; do
    case "$1" in
      -cpu|--cpu) DO_CPU=1 ;;
      -mem|--mem) DO_MEM=1 ;;
      -*) echo "unknown flag: $1" >&2; exit 1 ;;
      *) args+=("$1") ;;
    esac
    shift
  done
  [ "$DO_CPU" -eq 0 ] && [ "$DO_MEM" -eq 0 ] && { DO_CPU=1; DO_MEM=1; }

  # With explicit phases, honor them; otherwise mem (and both) walk the full
  # scenario, while cpu-only runs just the heavy/idle capture windows plus the
  # connect/end needed to reach and tear them down.
  local phases=()
  if [ ${#args[@]} -gt 0 ]; then
    phases=("${args[@]}")
  elif [ "$DO_MEM" -eq 1 ]; then
    phases=(baseline connect heavy drain idle toggle end)
  else
    phases=(connect heavy idle end)
  fi

  local profiling="mem+cpu"
  [ "$DO_CPU" -eq 0 ] && profiling="mem"
  [ "$DO_MEM" -eq 0 ] && profiling="cpu"

  mkdir -p "$OUTDIR"
  PHASE_FILE="$OUTDIR/.phase"; : > "$PHASE_FILE"
  SAMPLES_CSV="$OUTDIR/samples.csv"
  CAPTURES_CSV="$OUTDIR/captures.csv"
  echo "ts,phase,rss_kb,hwm_kb,threads,active_conns,up_bps,down_bps" > "$SAMPLES_CSV"
  echo "ts,label,rss_kb,hwm_kb,threads,active_conns" > "$CAPTURES_CSV"
  write_samplerlib

  trap cleanup EXIT
  trap 'cleanup; trap - INT; kill -INT $$' INT
  trap 'cleanup; trap - TERM; kill -TERM $$' TERM
  log "output dir: $OUTDIR  worker pid: $(worker_pid)  profile: $profiling  phases: ${phases[*]}"
  start_sampler

  local ph
  for ph in "${phases[@]}"; do
    case "$ph" in
      baseline) phase_baseline ;;
      connect)  phase_connect ;;
      heavy)    phase_heavy ;;
      drain)    phase_drain ;;
      idle)     phase_idle ;;
      toggle)   phase_toggle ;;
      end)      phase_end ;;
      *) log "unknown phase: $ph (skipped)" ;;
    esac
  done

  printf '\nDone. Artifacts in %s/\n' "$OUTDIR"
  echo "  samples.csv     RSS/threads/throughput time series, tagged by phase"
  echo "  captures.csv    one row per checkpoint (rss/hwm/threads/conns)"
  [ "$OS" = Darwin ] && echo "  note: macOS has no VmHWM, so hwm_kb is a sampled running-max and can understate the true peak"
  if [ "$DO_MEM" -eq 1 ]; then
    echo "  heap.*.pprof    live-set heap (GC forced before sampling)"
    echo "  goroutine.*     goroutine stacks (leak detection)"
    echo "  allocs.*        cumulative allocation profile (GC pressure)"
  fi
  [ "$DO_CPU" -eq 1 ] && echo "  cpu.*.pprof     CPU profile spanning the heavy/idle windows"

  if [ "$DO_MEM" -eq 1 ]; then
    printf '\nSuggested memory analysis (heap inuse is live-set, captured with gc=1):\n'
    echo "  go tool pprof -top                                 $OUTDIR/heap.heavy_peak.pprof"
    echo "  go tool pprof -base heap.baseline_connected.pprof  $OUTDIR/heap.heavy_peak.pprof   # working set under load"
    echo "  go tool pprof -base heap.baseline_connected.pprof  $OUTDIR/heap.drain.pprof        # retained after drain == leak"
    echo "  go tool pprof -base heap.baseline_connected.pprof  $OUTDIR/heap.toggle.pprof       # teardown leak across cycles"
    echo "  go tool pprof -base goroutine.baseline_connected.pprof $OUTDIR/goroutine.drain.pprof  # leaked goroutines"
  fi
  if [ "$DO_CPU" -eq 1 ]; then
    printf '\nSuggested CPU analysis:\n'
    echo "  go tool pprof -top  $OUTDIR/cpu.heavy.pprof   # where CPU goes under load"
    echo "  go tool pprof -top  $OUTDIR/cpu.idle.pprof    # steady-state/background CPU"
  fi
}

main "$@"
