#!/usr/bin/env bash
# Native Linux (Lima) reproduction for the utreexo position map L1 benchmark.
#
# Target: macOS arm64 host + Ubuntu 24.04 arm64 guest (Lima, vz). The guest uses
# kernel cgroup v2 and an ext4 root disk, so /proc counters, MAP_POPULATE,
# FADV_DONTNEED and the page-cache cap all behave like production.
#
#   brew install lima
#   limactl start --name=posmap-bench --vm-type=vz --cpus=4 --memory=3 \
#     --disk=40 --tty=false template://ubuntu-24.04
#   bash scripts/run_l1_native.sh
#
# Guest RAM is deliberately small (3GiB). The cgroup MemoryMax cap (default 2G)
# is what forces page-cache eviction and reproduces the random-I/O wall. The
# 55GB posmap files are sparse: real disk use ≈ 8KiB * N per run.
set -euo pipefail

VM=${VM:-posmap-bench}
GO_VER=${GO_VER:-go1.25.14}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
GH=${GUEST_HOME:-/home/jm.guest}   # guest home lives on the VM disk (not host mount)
BENCH="$GH/bench"
SRC="$GH/bench-l1"
OUT="$GH/bench/l1-native.jsonl"

# Heuristic: N * 8KiB (~dirty/writeback volume) should exceed the cgroup budget.
# N=262144, M=2G  =>  working set ~2GiB  =>  1x budget (I/O bound, reproducible).
N=${N:-262144}
MEM=${MEM:-2G}

limactl list "$VM" >/dev/null 2>&1 || {
  echo "VM '$VM' not found. Create it first:" >&2
  echo "  limactl start --name=$VM --vm-type=vz --cpus=4 --memory=3 --disk=40 --tty=false template://ubuntu-24.04" >&2
  exit 1
}
limactl shell "$VM" -- true >/dev/null 2>&1 || limactl start "$VM" --tty=false

echo "==> guest: Go + v0.18.0 clone (idempotent)"
limactl shell "$VM" -- bash -s <<EOF
set -e
export PATH=\$PATH:/usr/local/go/bin
if [ ! -x /usr/local/go/bin/go ]; then
  cd /tmp
  curl -fsSLO https://go.dev/dl/${GO_VER}.linux-arm64.tar.gz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf ${GO_VER}.linux-arm64.tar.gz
fi
[ -d $SRC/.git ] || git clone --quiet https://github.com/utreexo/utreexo $SRC
git -C $SRC checkout -q v0.18.0
EOF

echo "==> copy bench file + build"
limactl copy "$REPO/internal/swisstable/bench_l1_test.go" "$VM:$SRC/internal/swisstable/bench_l1_test.go"
limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; mkdir -p $BENCH; cd $SRC && go test -c -o $BENCH/bench_l1.test ./internal/swisstable"

run() { # label n presize mix mem
  local label=$1 n=$2 pre=$3 mix=$4 mem=$5
  echo "==> $label (N=$n presize=$pre mix=$mix mem=$mem)"
  limactl shell "$VM" -- bash -c "
    rm -rf $BENCH/data/$label; sync; sleep 10
    sudo systemd-run --unit=l1bench --wait --pipe --collect \
      --property=MemoryMax=$mem --property=MemorySwapMax=0 \
      env BENCH_DIR=$BENCH/data/$label BENCH_N=$n BENCH_PRESIZE=$pre \
          BENCH_MIX=$mix BENCH_MODE=A BENCH_LABEL=$label BENCH_COLD=1 BENCH_OUT=$OUT \
      $BENCH/bench_l1.test -test.run '^\$' -test.bench '^BenchmarkL1Posmap\$' -test.benchtime 1x
    sync; sleep 5" | grep L1RESULT || true
}

limactl shell "$VM" -- bash -c "rm -f $OUT"
for mix in add 1:1; do
  run "p32-$mix"   "$N" 4294967296 "$mix" "$MEM"   # baseline: 2^32 presize (55GB)
  run "sized-$mix" "$N" $((2 * N))  "$mix" "$MEM"  # ~2N entries
  run "small-$mix" "$N" 65536       "$mix" "$MEM"  # forces x4 resize
done

echo "==> results"
mkdir -p "$REPO/results"
limactl shell "$VM" -- cat "$OUT" > "$REPO/results/l1-native.jsonl"
wc -l "$REPO/results/l1-native.jsonl"
