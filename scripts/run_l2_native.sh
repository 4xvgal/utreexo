#!/usr/bin/env bash
# L2 (Forest end-to-end) benchmark runner. See docs/bench-l2-report.md.
#
# Runs the production Forest through the public API:
#   OpenForest(dir, ExpectedMaxLeaves(n), MaxCacheMemory(m))
#   ModifyAndReturnTTLs(adds, dels, Proof{})
#
#   bash scripts/run_l2_native.sh
#
# RAM: guest 4GiB (Lima) + cgroup MemoryMax=2G. p32 runs are I/O bound and must
# run SEQUENTIALLY: two 2G scopes exceed the 4GiB guest and share one virtual
# disk, which distorts the measurement.
#
# Disk: an over-presized run writes ~13.7GB into the sparse 55GB posmap. Run
# dirs are root-owned (systemd-run), so clean them with sudo; a 40GB guest runs
# out after ~3 p32 runs.
set -euo pipefail

VM=${VM:-posmap-bench}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
GH=${GUEST_HOME:-/home/jm.guest}
BENCH="$GH/bench"
SRC="$GH/bench-l1"
OUT="$GH/bench/l2-results.jsonl"

N=${N:-1680000}
BLOCKS=${BLOCKS:-10000}
MEM=${MEM:-2G}

limactl list "$VM" >/dev/null 2>&1 || { echo "create the VM first (see run_l1_native.sh)"; exit 1; }
limactl shell "$VM" -- true >/dev/null 2>&1 || limactl start "$VM" --tty=false

limactl shell "$VM" -- mkdir -p "$SRC/internal/l2bench"
for f in internal/l2bench/bench_l2_test.go go.mod go.sum; do
  limactl copy "$REPO/$f" "$VM:$SRC/$f"
done
limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; mkdir -p $BENCH; cd $SRC && go mod download && go test -c -o $BENCH/l2bench.test ./internal/l2bench"

run() { # label n blocks delPct presize cacheMB forceRebuild
  local label=$1 n=$2 blocks=$3 del=$4 pre=$5 cache=$6 rebuild=$7
  echo "==> $label (del=$del% presize=$pre cache=${cache}MB rebuild=$rebuild)"
  limactl shell "$VM" -- bash -c "
    sudo rm -rf $BENCH/data/$label; sync; sleep 8
    sudo systemd-run --unit=l2bench --wait --pipe --collect \
      --property=MemoryMax=$MEM --property=MemorySwapMax=0 \
      env BENCH_L2_DIR=$BENCH/data/$label BENCH_L2_N=$n BENCH_L2_BLOCKS=$blocks \
          BENCH_L2_DEL_PCT=$del BENCH_L2_PRESIZE=$pre BENCH_L2_CACHE_MB=$cache \
          BENCH_L2_REOPEN=1 BENCH_L2_FORCE_REBUILD=$rebuild \
          BENCH_L2_LABEL=$label BENCH_L2_OUT=$OUT \
      $BENCH/l2bench.test -test.run '^\$' -test.bench '^BenchmarkL2Forest\$' -test.benchtime 1x \
      2>&1 | grep -E 'L2RESULT|FAIL|panic'
    sync; sleep 5" || true
}

limactl shell "$VM" -- bash -c "rm -f $OUT"
# sized (fast, cache-resident)
run l2-sized-del0           "$N" "$BLOCKS" 0  $((2 * N)) 64  0
run l2-sized-del30          "$N" "$BLOCKS" 30 $((2 * N)) 64  0
run l2-sized-del30-c512     "$N" "$BLOCKS" 30 $((2 * N)) 512 0
run l2-sized-del30-rebuild  "$N" "$BLOCKS" 30 $((2 * N)) 64  1
# failure point (I/O bound, ~15 min each)
run l2-p32-del0             "$N" "$BLOCKS" 0  4294967296 64  0
run l2-p32-del30            "$N" "$BLOCKS" 30 4294967296 64  0

echo "==> results"
mkdir -p "$REPO/results"
limactl shell "$VM" -- cat "$OUT" > "$REPO/results/l2-results.jsonl"
wc -l "$REPO/results/l2-results.jsonl"
