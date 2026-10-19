#!/usr/bin/env bash
# P3: L2 (Forest end-to-end) benchmark with the bench-only "eh" position map,
# plus the EH regression test. Runs only in the eh-bench VM / bench-eh tree;
# never touches mainnet data, utreexod, or the docker compose project.
#
#   bash scripts/run_eh_l2.sh p3       # L2 matrix
#   bash scripts/run_eh_l2.sh regtest  # forest EH==swiss regression + go test
set -euo pipefail

VM=${VM:-eh-bench}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
GH=${GUEST_HOME:-/home/jm.guest}
EB="$GH/eh-bench"
SRC="$EB/l2src"
DATA="$EB/l2data"
OUT="$EB/out/eh-l2.jsonl"
PHASE=${1:-p3}
N=${N:-1680000}
BLOCKS=${BLOCKS:-10000}
MEM=${MEM:-2G}

limactl list "$VM" >/dev/null 2>&1 || { echo "create VM $VM first"; exit 1; }
limactl shell "$VM" -- true >/dev/null 2>&1 || limactl start "$VM" --tty=false

echo "==> copy full worktree (no mainnet data, no .git/results)"
limactl shell "$VM" -- bash -c "rm -rf $SRC && mkdir -p $SRC $DATA $EB/out"
tar -C "$REPO" --exclude=.git --exclude=results --exclude='*.test' --exclude='._*' --exclude=.DS_Store -cf - . \
  | limactl shell "$VM" -- bash -c "tar -C $SRC -xf - && find $SRC -name '._*' -delete && find $SRC -name '.DS_Store' -delete"

echo "==> build l2bench"
limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; cd $SRC && go mod download && go test -c -o $EB/l2bench.test ./internal/l2bench"

l2run() { # label impl del presize cache rebuild mem
  local label=$1 impl=$2 del=$3 pre=$4 cache=$5 rebuild=$6 mem=$7
  echo "==> L2 $label (impl=$impl del=$del% pre=$pre cache=${cache}MB rebuild=$rebuild mem=$mem)"
  limactl shell "$VM" -- bash -c "
    sudo systemctl stop l2bench 2>/dev/null || true; sudo systemctl reset-failed l2bench 2>/dev/null || true
    sudo rm -rf $DATA/$label; sync; sleep 5
    sudo systemd-run --unit=l2bench --wait --pipe --collect \
      --property=MemoryMax=$mem --property=MemorySwapMax=0 --property=RuntimeMaxSec=${L2_TIMEOUT:-1200} \
      env BENCH_L2_DIR=$DATA/$label BENCH_L2_N=$N BENCH_L2_BLOCKS=$BLOCKS \
          BENCH_L2_DEL_PCT=$del BENCH_L2_PRESIZE=$pre BENCH_L2_CACHE_MB=$cache \
          BENCH_L2_IMPL=$impl BENCH_L2_REOPEN=1 BENCH_L2_FORCE_REBUILD=$rebuild \
          BENCH_L2_LABEL=$label BENCH_L2_OUT=$OUT \
      $EB/l2bench.test -test.run '^\$' -test.bench '^BenchmarkL2Forest\$' -test.benchtime 1x \
      2>&1 | grep -E 'L2RESULT|FAIL|panic' || true
    sync; sleep 3"
}

case "$PHASE" in
p3)
  pre=$((2 * N))
  l2run eh-del0        eh    0  "$pre" 64 0  "$MEM"
  l2run eh-del30       eh    30 "$pre" 64 0  "$MEM"
  l2run eh-del30-c512  eh    30 "$pre" 512 0 "$MEM"
  l2run eh-del30-reb   eh    30 "$pre" 64 1  "$MEM"
  l2run lh-del30       lh    30 "$pre" 64 0  "$MEM"
  l2run swiss-del30    swiss 30 "$pre" 64 0  "$MEM"
  ;;
regtest)
  echo "==> EH forest regression (EH==swiss roots, reopen)"
  limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; cd $SRC && go test ./... -run 'TestEHPositionMapMatchesSwiss' -count=1 -v 2>&1 | tail -20"
  echo "==> full repo regression"
  limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; cd $SRC && go test ./... -count=1 2>&1 | tail -40"
  ;;
*)
  echo "usage: $0 p3|regtest" >&2
  exit 1
  ;;
esac

echo "==> pull l2 results"
mkdir -p "$REPO/results"
limactl shell "$VM" -- bash -c "test -f $OUT && cat $OUT" > "$REPO/results/eh-l2.jsonl" || true
python3 - "$REPO" <<'PY'
import json, os, subprocess, sys
root = sys.argv[1]
c = subprocess.check_output(["git", "-C", root, "rev-parse", "HEAD"]).decode().strip()
p = os.path.join(root, "results", "eh-l2.jsonl")
if os.path.exists(p):
    seen = {}
    order = []
    for ln in open(p):
        ln = ln.strip()
        if not ln:
            continue
        d = json.loads(ln)
        d["commit"] = c
        if d["label"] not in seen:
            order.append(d["label"])
        seen[d["label"]] = json.dumps(d, separators=(",", ":"))
    open(p, "w").write("\n".join(seen[k] for k in order) + "\n")
    print("l2 rows:", len(order))
PY
