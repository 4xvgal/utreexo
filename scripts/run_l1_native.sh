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
#
# Implementations (BENCH_IMPL):
#   swiss  production SwissPositionMap (BENCH_PRESIZE controls the presize)
#   lh     linear hashing (one bucket split at a time, no presize)
#   bbolt  go.etcd.io/bbolt B+tree (stores keys, no verifyHash)
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
# The real failure point (H~178k) is N=1680000 with M=2G (working set ~13GiB).
N=${N:-262144}
MEM=${MEM:-2G}
MIXES=${MIXES:-"add 2:1"}   # 1:1 is degenerate: the balanced random walk keeps the live set ~sqrt(N)

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

echo "==> copy sources + build"
for f in internal/swisstable/bench_l1_test.go internal/swisstable/bench_l1_alt_test.go go.mod go.sum; do
  limactl copy "$REPO/$f" "$VM:$SRC/$f"
done
limactl shell "$VM" -- bash -c "export PATH=\$PATH:/usr/local/go/bin; mkdir -p $BENCH; cd $SRC && go mod download && go test -c -o $BENCH/bench_l1.test ./internal/swisstable"

run() { # label n impl presize mix mem
  local label=$1 n=$2 impl=$3 pre=$4 mix=$5 mem=$6
  echo "==> $label (N=$n impl=$impl presize=$pre mix=$mix mem=$mem)"
  limactl shell "$VM" -- bash -c "
    rm -rf $BENCH/data/$label; sync; sleep 10
    sudo systemd-run --unit=l1bench --wait --pipe --collect \
      --property=MemoryMax=$mem --property=MemorySwapMax=0 \
      env BENCH_IMPL=$impl BENCH_DIR=$BENCH/data/$label BENCH_N=$n BENCH_PRESIZE=$pre \
          BENCH_MIX=$mix BENCH_MODE=A BENCH_LABEL=$label BENCH_COLD=1 BENCH_OUT=$OUT \
      $BENCH/bench_l1.test -test.run '^\$' -test.bench '^BenchmarkL1Posmap\$' -test.benchtime 1x
    sync; sleep 5" | grep L1RESULT || true
}

limactl shell "$VM" -- bash -c "rm -f $OUT"
for mix in $MIXES; do
  run "swiss-p32-$mix"   "$N" swiss 4294967296 "$mix" "$MEM"   # baseline: 2^32 presize (55GB)
  run "swiss-sized-$mix" "$N" swiss $((2 * N))    "$mix" "$MEM" # ~2N entries
  run "lh-$mix"          "$N" lh    $((2 * N))    "$mix" "$MEM"
  run "bbolt-$mix"       "$N" bbolt $((2 * N))    "$mix" "$MEM"
done

echo "==> results"
mkdir -p "$REPO/results"
limactl shell "$VM" -- cat "$OUT" > "$REPO/results/l1-native.jsonl"
wc -l "$REPO/results/l1-native.jsonl"
