#!/usr/bin/env bash
#
# stress-core.sh - peg N logical cores at ~100% so the sysmon-agent dashboard's
# "CPU core busy count" (CPUCoreSet.Busy, the `busy N/M` readout) can be tested.
#
# The dashboard counts a core as "busy" at >=80% utilization (cpuCoreBusyPercent
# in metrics.go). On Linux per-core utilization comes from the sampler's FAST
# lane (~5 Hz, -fast-ms), so a pegged core shows up within about a second.
#
# Two knobs model the workload shape, independent of how many cores it lights up:
#   -n/--cores N    the pinned CPU-affinity window (cores the OS may schedule on)
#   -t/--threads T  how many worker threads the simulated program runs
# Threads are spread round-robin across the N pinned cores, so a core only needs
# one thread to read 100% -- more threads on the same core add contention but do
# NOT raise the busy count. The number of cores driven to >=80% is therefore
# min(N, T):
#   -n 1 -t 1   single-threaded program        -> busy 1/<cores>
#   -n 4 -t 4   thread pool, one core each     -> busy 4/<cores>
#   -n 2 -t 8   oversubscribed (4x threads)     -> busy 2/<cores>  (cores pegged,
#                                                               threads contend)
#   -n 4 -t 2   wide affinity, only 2 threads  -> busy 2/<cores>
# That matrix is exactly what the busy-count feature exists to distinguish from
# the aggregate cpu_percent average, which hides all of it behind ~6% per core.
#
# Usage:
#   ./stress-core.sh               # 1 thread, 1 core -> `busy 1/<cores>`
#   ./stress-core.sh -n 4          # 4 threads, 4 cores -> `busy 4/<cores>`
#   ./stress-core.sh -n 2 -t 8     # oversubscribed -> `busy 2/<cores>`
#   ./stress-core.sh -n 2 -c 8     # pin to cores 8 and 9 (default: cores 0..N-1)
#   ./stress-core.sh --seconds 10  # auto-stop after 10s (default: run until Ctrl+C)
#
# Options:
#   -n, --cores N        size of the pinned CPU-affinity window (default: 1)
#   -t, --threads T      number of busy worker threads (default: one per core,
#                        i.e. T = N, the classic one-loop-per-core shape)
#   -c, --cpu-start IDX  logical CPU index to pin the first core to; the window
#                        covers IDX, IDX+1, ... (default: 0). Ignored if taskset
#                        is unavailable.
#   -s, --seconds SECS   stop after SECS seconds instead of waiting for Ctrl+C
#   -h, --help           show this help
#
# Requires: bash, (optionally) taskset from util-linux. Without taskset the
# loops still run and burn CPU, but they are not pinned -- the scheduler may
# migrate them, so the busy count can flicker between cores.
set -euo pipefail

NUM=1
THREADS=0
CPU_START=0
SECONDS_OPT=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--cores)     NUM="${2:?--cores needs a value}"; shift 2 ;;
        -t|--threads)   THREADS="${2:?--threads needs a value}"; shift 2 ;;
        -c|--cpu-start) CPU_START="${2:?--cpu-start needs a value}"; shift 2 ;;
        -s|--seconds)   SECONDS_OPT="${2:?--seconds needs a value}"; shift 2 ;;
        -h|--help)      sed -n '2,44p' "$0"; exit 0 ;;
        *) echo "unknown argument: $1" >&2; sed -n '2,44p' "$0" >&2; exit 2 ;;
    esac
done

if ! [[ "$NUM" =~ ^[0-9]+$ ]] || [[ "$NUM" -lt 1 ]]; then
    echo "--cores must be a positive integer" >&2; exit 2
fi
if ! [[ "$CPU_START" =~ ^[0-9]+$ ]]; then
    echo "--cpu-start must be a non-negative integer" >&2; exit 2
fi
# -t/--threads defaults to one worker per pinned core so the script's classic
# default shape (one busy loop per core) is unchanged.
if [[ "$THREADS" -eq 0 ]]; then
    THREADS="$NUM"
fi
if ! [[ "$THREADS" =~ ^[0-9]+$ ]] || [[ "$THREADS" -lt 1 ]]; then
    echo "--threads must be a positive integer" >&2; exit 2
fi
# Expected busy cores = min(N, T): every pinned core only if there are at least
# N threads, else the first T cores (round-robin placement). Oversubscription
# (T > N) cannot raise the count -- the extra threads just contend on already-
# saturated cores.
if (( NUM < THREADS )); then
    EFFECTIVE="$NUM"
else
    EFFECTIVE="$THREADS"
fi

TOTAL_CORES="$(nproc 2>/dev/null || echo 1)"
if (( CPU_START + NUM > TOTAL_CORES )); then
    echo "requested cores $CPU_START..$((CPU_START + NUM - 1)) exceed $TOTAL_CORES logical CPUs" >&2
    exit 2
fi

# taskset pins each busy loop to one logical CPU so the busy count is exact.
HAVE_TASKSET=0
if command -v taskset >/dev/null 2>&1; then
    HAVE_TASKSET=1
fi

PIDS=()
cleanup() {
    # Kill every burner we spawned, then reap. Sends TERM first so a stuck loop
    # still exits; escalate to KILL if anything lingers after 1s.
    for pid in "${PIDS[@]:-}"; do
        kill "$pid" 2>/dev/null || true
    done
    for pid in "${PIDS[@]:-}"; do
        local dead=0
        for _ in {1..10}; do
            kill -0 "$pid" 2>/dev/null || { dead=1; break; }
            sleep 0.1
        done
        [[ "$dead" -eq 1 ]] || kill -9 "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
    echo
    echo "==> stopped; busy count should drop back to 0/$TOTAL_CORES"
}
trap cleanup EXIT INT TERM

echo "==> pegging $NUM core(s) at ~100% with $THREADS worker thread(s) to exercise CPUCoreSet.Busy (threshold 80%)"
if [[ "$HAVE_TASKSET" -eq 1 ]]; then
    for ((t = 0; t < THREADS; t++)); do
        cpu=$((CPU_START + (t % NUM)))
        taskset -c "$cpu" bash -c 'while :; do :; done' &
        PIDS+=("$!")
        echo "    thread $t -> core $cpu  (pid ${PIDS[-1]})"
    done
else
    echo "    (taskset not found -- loops will not be pinned; busy count may flicker)" >&2
    for ((t = 0; t < THREADS; t++)); do
        bash -c 'while :; do :; done' &
        PIDS+=("$!")
        echo "    unpinned thread $t  (pid ${PIDS[-1]})"
    done
fi

echo "==> expect dashboard / /api/metrics to show: busy $EFFECTIVE/$TOTAL_CORES"
echo "    (Ctrl+C to stop$( [[ -n "$SECONDS_OPT" ]] && echo ", or wait ${SECONDS_OPT}s" ))"

if [[ -n "$SECONDS_OPT" ]]; then
    if ! [[ "$SECONDS_OPT" =~ ^[0-9]+$ ]]; then
        echo "--seconds must be a non-negative integer" >&2; exit 2
    fi
    sleep "$SECONDS_OPT"
else
    wait
fi
