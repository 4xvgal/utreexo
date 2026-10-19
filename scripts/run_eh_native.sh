#!/usr/bin/env bash
# EH position-map benchmark on the dedicated Lima VM "eh-bench".
#
#   bash scripts/run_eh_native.sh p0|p1|p1b|p2|all
#
# Every artefact stays under the guest's $HOME/eh-bench. The host worktree is
# copied in (no clone): internal/eh, internal/swisstable, go.mod, go.sum.
set -euo pipefail

VM=${VM:-eh-bench}
GO_VER=${GO_VER:-go1.25.14}
PHASE=${1:-all}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
GH=${GUEST_HOME:-/home/jm.guest}
BENCH="$GH/eh-bench"
SRC="$BENCH/src"
OUT="$BENCH/out/eh-l1.jsonl"
WALL="$BENCH/out/eh-wall.jsonl"
SHARED="$BENCH/data-shared"
COMMIT=$(git -C "$REPO" rev-parse HEAD)

limactl list "$VM" >/dev/null 2>&1 || {
  echo "VM '$VM' not found. Create it first:" >&2
  echo "  limactl start --name=$VM --vm-type=vz --cpus=4 --memory=4 --disk=60 --tty=false template://ubuntu-24.04" >&2
  exit 1
}
limactl shell "$VM" -- true >/dev/null 2>&1 || limactl start "$VM" --tty=false

echo "==> guest: Go + dirs"
limactl shell "$VM" -- bash -s <<EOF
set -e
export PATH=\$PATH:/usr/local/go/bin
if [ ! -x /usr/local/go/bin/go ]; then
  cd /tmp
  curl -fsSLO https://go.dev/dl/${GO_VER}.linux-arm64.tar.gz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf ${GO_VER}.linux-arm64.tar.gz
fi
mkdir -p $SRC $BENCH/out $SHARED $BENCH/data
EOF

echo "==> copy sources (commit $COMMIT)"
tar -C "$REPO" -cf - internal/eh internal/swisstable go.mod go.sum \
  | limactl shell "$VM" -- bash -c "tar -C $SRC -xf -"

echo "==> build bench binary"
limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; cd $SRC && go mod download && go test -c -o $BENCH/bench_l1.test ./internal/swisstable"

run() { # label n impl presize mix mem [extra env...]
  local label=$1 n=$2 impl=$3 pre=$4 mix=$5 mem=$6
  shift 6
  local extra="$*"
  echo "==> $label (N=$n impl=$impl presize=$pre mix=$mix mem=$mem $extra)"
  limactl shell "$VM" -- bash -c "
    sudo systemctl reset-failed ehbench 2>/dev/null || true; sudo rm -rf $BENCH/data/$label; sync; sleep 10
    sudo systemd-run --unit=ehbench --wait --pipe --collect \
      --property=MemoryMax=$mem --property=MemorySwapMax=0 \
      --property=RuntimeMaxSec=${RUN_TIMEOUT:-180} \
      env BENCH_IMPL=$impl BENCH_DIR=$BENCH/data/$label BENCH_N=$n BENCH_PRESIZE=$pre \
          BENCH_MIX=$mix BENCH_MODE=A BENCH_LABEL=$label BENCH_COLD=1 BENCH_OUT=$OUT \
          BENCH_DATA=$SHARED/data-$n.dat EH_SAMPLES=$BENCH/out/eh-samples-$label.jsonl \
          BENCH_DEBUG=${BENCH_DEBUG:-1} $extra \
      $BENCH/bench_l1.test -test.run '^\$' -test.bench '^BenchmarkL1Posmap\$' -test.benchtime 1x
    sync; sleep 5" | grep -E 'L1RESULT|EH_STATS|Killed|signal' || true
}

wall() { # label n impl mem [extra env...]
  local label=$1 n=$2 impl=$3 mem=$4
  shift 4
  local extra="$*"
  echo "==> WALL $label (N=$n impl=$impl mem=$mem $extra)"
  limactl shell "$VM" -- bash -c "
    sudo systemctl stop ehwall 2>/dev/null || true; sudo systemctl reset-failed ehwall 2>/dev/null || true; sudo rm -rf $BENCH/data/$label; sync; sleep 10
    sudo systemd-run --unit=ehwall --wait --pipe --collect \
      --property=MemoryMax=$mem --property=MemorySwapMax=0 \
      --property=RuntimeMaxSec=${WALL_TIMEOUT:-900} \
      env BENCH_IMPL=$impl BENCH_DIR=$BENCH/data/$label BENCH_N=$n \
          BENCH_MIX=add BENCH_LABEL=$label BENCH_COLD=1 BENCH_OUT=$WALL \
          BENCH_DATA=$SHARED/data-$n.dat EH_SAMPLES=$BENCH/out/eh-samples-$label.jsonl \
          BENCH_DEBUG=${BENCH_DEBUG:-1} $extra \
      $BENCH/bench_l1.test -test.run '^\$' -test.bench '^BenchmarkL1Wall\$' -test.benchtime 1x
    sync; sleep 5" | grep -E 'L1RESULT|EH_STATS|Killed|signal' || true
}

case "$PHASE" in
p0)
  limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; cd $SRC && go test ./internal/eh -run 'TestEH' -count=1"
  ;;
p1)
  limactl shell "$VM" -- bash -c "rm -f $OUT"
  NS=${NS:-"1000000 4000000 16000000"}
  for n in $NS; do
    run "swiss-2n-$n"   "$n" swiss $((2*n))        add 3G
    run "lh-$n"         "$n" lh    $((2*n))        add 3G
    run "eh04-$n"       "$n" eh    "$n"            add 3G EH_LOAD=0.40
    run "eh08-$n"       "$n" eh    "$n"            add 3G EH_LOAD=0.80
  done
  # swiss presize 2^32 = 55GB sparse + MAP_POPULATE. It cannot run in the
  # uncapped P1 config (it would fault in 55GB), so it is measured under a
  # 2G cap, the same condition as the earlier failpoint run (fp-p32-1680k).
  # A presize sweep at fixed N gives the ops/s(map_bytes) curve for
  # extrapolating the larger N columns.
  run "swiss-p32-1000000" 1000000 swiss 4294967296 add 2G || true
  run "swiss-p16x-1000000" 1000000 swiss 16777216 add 2G || true
  run "swiss-p128x-1000000" 1000000 swiss 134217728 add 2G || true
  ;;
p1rest)
  # Resume/finish P1 without truncating the JSONL.
  RUN_TIMEOUT=${RUN_TIMEOUT:-300}
  run "eh04-16000000" 16000000 eh 16000000 add 3G EH_LOAD=0.40
  run "eh08-16000000" 16000000 eh 16000000 add 3G EH_LOAD=0.80
  run "swiss-p16x-1000000" 1000000 swiss 16777216 add 2G || true
  run "swiss-p128x-1000000" 1000000 swiss 134217728 add 2G || true
  ;;
p1lh51)
  NS=${NS:-"1000000 4000000 16000000"}
  for n in $NS; do
    run "lh51-$n" "$n" lh51 "$((2*n))" add 3G
  done
  ;;
p2lh51)
  P2N=${P2N:-"8000000 16000000"}
  for n in $P2N; do
    for rep in 1 2 3; do
      wall "lh51-512m-$n-r$rep" "$n" lh51 512M
    done
  done
  ;;
p1b)
  for n in 4000000 16000000; do
    for mix in 2:1 1:1; do
      run "lh-$mix-$n"   "$n" lh $((2*n)) "$mix" 3G
      run "eh08-$mix-$n" "$n" eh "$n"     "$mix" 3G EH_LOAD=0.80
    done
  done
  ;;
p2|p2a|p2b)
  if [ "$PHASE" = p2a ]; then P2N="8000000"; fi
  if [ "$PHASE" = p2b ]; then P2N="16000000"; fi
  P2N=${P2N:-"8000000 16000000"}
  for n in $P2N; do
    for rep in 1 2 3; do
      for spec in "lh:3G:" "eh04:3G:EH_LOAD=0.40" "eh08:3G:EH_LOAD=0.80" "eh09:3G:EH_LOAD=0.90"; do
        impl=${spec%%:*}; rest=${spec#*:}; mem=${rest%%:*}; extra=${rest#*:}
        wall "$impl-512m-$n-r$rep" "$n" "$impl" 512M $extra
      done
    done
  done
  ;;
all)
  bash "$0" p1
  bash "$0" p1b
  bash "$0" p2
  ;;
*)
  echo "usage: $0 p0|p1|p1b|p2|all" >&2
  exit 1
  ;;
esac

echo "==> pull results"
mkdir -p "$REPO/results"
limactl shell "$VM" -- tar -C "$BENCH/out" -cf - . 2>/dev/null | tar -C "$REPO/results" -xf - || true
# Stamp the commit into every result line.
python3 - "$COMMIT" "$REPO/results" <<'PY'
import json, os, sys
commit, root = sys.argv[1], sys.argv[2]
for name in ("eh-l1.jsonl", "eh-wall.jsonl"):
    p = os.path.join(root, name)
    if not os.path.exists(p):
        continue
    out = []
    for line in open(p):
        line = line.strip()
        if not line:
            continue
        try:
            d = json.loads(line)
        except Exception:
            continue
        d["commit"] = commit
        out.append(json.dumps(d, separators=(",", ":")))
    open(p, "w").write("\n".join(out) + ("\n" if out else ""))
PY
wc -l "$REPO/results/eh-l1.jsonl" "$REPO/results/eh-wall.jsonl" 2>/dev/null || true
