# EH position map 벤치 결과

## (1) 한 줄 결론

메모리 벽(512M 캡, N=16M)에서 **EH(0.8)는 map 452MB로 캡 안에 머물며 325k ops/s를 유지**했고,
bench lh는 map 604MB로 캡을 넘어 101k ops/s로 꺾였다(H2 지지). 그러나 hot 구간(N=1M)에서는
EH(0.8)가 LH의 **0.30×**로 느려 H3는 기각됐고, A1은 production LH(0.4) 기준 0.54로 통과하지만
in-harness bench lh 기준 0.75로 미달한다(조건부).

## (2) 자원 / 커밋 / VM

- host worktree: `/Volumes/mx500/utreexo-lab/bench-eh` (branch `poc/eh-v1`, base `aad6c84`)
- host 판정: `git -C bench-eh rev-parse HEAD` (결과 JSONL의 `commit` 필드)
- VM: `eh-bench`, vz, 4 vCPU, **4GiB RAM**, 60GiB disk, Ubuntu 24.04 arm64 (kernel 6.8), Go 1.25.14
  - 지시에 따라 초기 8GiB에서 4GiB로 축소.
- cgroup: P1/P1b `MemoryMax=3G`, P2 `MemoryMax=512M`, 모두 `MemorySwapMax=0`,
  `systemd-run` transient scope.
- 데이터: sparse `Truncate` + per-run `sudo rm -rf`는 `$HOME/eh-bench` 하위만.

## (3) P0 결과 (정확성/불변식)

`go test ./internal/eh` (N=1M, load 0.4/0.7/0.8/0.9 + eh8) — **전부 PASS**:

- 삽입 후 `Count()==N`, 임의 1000키 Get hit, 미삽입 1000키 miss.
- 절반 Delete 후 `Count()==N/2`, 삭제 키 miss / 생존 키 hit.
- 디렉터리 불변식: `dirSlots==1<<D`, 모든 슬롯이 유효 버킷, `localDepth<=D`,
  버킷 엔트리의 상위 d비트 == 슬롯 prefix, 버킷당 엔트리 <=128.
- `ForEach` 순회 수 == `Count()`. **`placeFull==0` (전 구간)**.
- eh8(8B slot + verifyHash)도 동일 통과.

## (4) P1 표 (hot, cap 3G, mix=add, cold)

| N | impl | map (MB) | B/entry | ops/s | setup (s) | majflt |
|---|---|---|---|---|---|---|
| 1M | swiss(2N) | 25.71 | 25.71 | 3,735,831 | 0.00 | 0 |
| 1M | lh | 37.75 | 37.75 | 2,821,490 | 0.01 | 0 |
| 1M | eh(0.4) | 53.27 | 53.27 | 1,063,717 | 0.00 | 519 |
| 1M | eh(0.8) | 28.36 | 28.36 | 848,791 | 0.00 | 237 |
| 4M | swiss(2N) | 102.86 | 25.71 | 2,863,528 | 0.02 | 0 |
| 4M | lh | 151.00 | 37.75 | 1,962,883 | 0.03 | 0 |
| 4M | eh(0.4) | 211.70 | 52.92 | 832,562 | 0.00 | 2,134 |
| 4M | eh(0.8) | 113.42 | 28.36 | 802,140 | 0.00 | 939 |
| 16M | swiss(2N) | 411.43 | 25.71 | 1,030,221 | 0.08 | 0 |
| 16M | lh | 603.98 | 37.75 | 1,671,659 | 0.17 | 0 |
| 16M | eh(0.4) | 844.71 | 52.79 | 337,173 | 0.00 | 8,564 |
| 16M | eh(0.8) | 452.30 | 28.27 | 524,834 | 0.00 | 3,675 |

- EH map은 `21B/load` 법칙을 따른다: eh(0.4)=52.5~53.3 B/e, eh(0.8)=28.3 B/e → **1.88× 차이** (H1).
- 실측 load: eh(0.4)=0.400 (splits 0), eh(0.8)=0.747 (splits 36,182 @16M), `placeFull=0`.
- EH directory bytes: 143KB @1M(0.51%), 2.27MB @16M(0.50%) → A5.
- write_bytes: EH 16M 삽입에서 26.2GB(eh08)/48.5GB(eh04). bench lh는 0.30GB.
  EH의 pwrite(1B ctrl + 20B slot)가 4KB 페이지를 반복 dirty 시키는 비용.

## (5) P1b 표 (churn, cap 3G)

| N | mix | impl | map (MB) | B/entry | ops/s | load | tombRatio | splits |
|---|---|---|---|---|---|---|---|---|
| 4M | 2:1 | lh | 151.00 | 37.75 | 2,395,160 | – | – | 0 |
| 4M | 2:1 | eh(0.8) | 88.64 | 22.16 | 931,136 | 0.477 | 1.6% | **0** |
| 4M | 1:1 | lh | 151.00 | 37.75 | 8,019,496 | – | – | 0 |
| 4M | 1:1 | eh(0.8) | 88.64 | 22.16 | 1,077,362 | 0 (live) | 100% of used | **0** |
| 16M | 2:1 | lh | 603.98 | 37.75 | 1,820,702 | – | – | 0 |
| 16M | 2:1 | eh(0.8) | 354.55 | 22.16 | 649,741 | 0.477 | 1.6% | **0** |
| 16M | 1:1 | lh | 603.98 | 37.75 | 7,268,099 | – | – | 0 |
| 16M | 1:1 | eh(0.8) | 354.55 | 22.16 | 823,131 | 0 (live) | 100% of used | **0** |

- **EH split 0회**: tombstone이 cascade/overflow split을 만들지 않았다 (설계대로 국소 split).
- 단 EH presize가 `expectedEntries=N` 기준이라 2:1(최종 live N/2)에서 load 0.477로 과소;
  steady-state 기준으로는 live에 맞춘 presize가 공정하다(미실행, 8절).

## (6) P2: 메모리 벽 (MEM=512M, mix=add, 스트리밍)

### 6a. run 요약 (3회 중앙값; EH04-16M은 300s에서 kill, 미완)

| N | impl | map (MB) | ops/s (med) | cgroup peak (MB) | majflt | read (GB) | 완료 |
|---|---|---|---|---|---|---|---|
| 8M | eh(0.8) | 226.2 | 444,524 | 230 | 1,843 | 0 | ✅ |
| 8M | eh(0.9) | 226.2 | 367,040 (1회) | 230 | 1,847 | 0 | ✅ (eh08과 동일) |
| 8M | eh(0.4) | 422.7 | 335,673 | 422 | 4,223 | 0 | ✅ |
| 8M | lh | 302.0 | 1,031,505 | 512 (캡) | 3,270 | 0.28 | ✅ |
| 16M | eh(0.8) | 452.3 | 325,163 | 453 | 3,671 | 0 | ✅ |
| 16M | lh | 604.0 | 100,604 | 512 (캡) | 1,692,559 | 111.6 | ✅ |
| 16M | eh(0.4) | 844.7 | ~20,000 | 512 (캡) | 2,280,383 | 195.3 | ❌ 6.3M/16M에서 kill |

- 8M: 모든 map이 캡 안(또는 경계) → EH08 445k vs LH 1.03M. LH는 cgroup 캡에 붙어
  data page cache reclaim이 시작되지만 8M 내에는 급락 없음.
- 16M: EH08 map 452MB < 512MB → majflt 3.7k, read 0, **무꺾임**. LH map 604MB > 캡 →
  read 111.6GB, majflt 1.69M. EH04 map 845MB > 캡 → read 195GB, majflt 2.28M,
  300s 안에 39%만 완료.

### 6b. 꺾임 지점 (per-second 샘플, `eh-samples-*.jsonl`)

| run | 구간 | ops | ops/s | majflt | read |
|---|---|---|---|---|---|
| lh 16M r1 | 시작 | 2.5M | 2,472k | 414 | 0.1GB |
| | | 4.5M | 2,002k | 950 | 0.1GB |
| | | 6.6M | 680k | 1,531 | 0.2GB |
| | | 10.4M | 553k | 3,372 | 0.3GB |
| | **꺾임 ~11–12M** | 12.0M | **15k** | 271,145 | 11.3GB |
| | | 14.0M | 33k | 917,193 | 53.7GB |
| | 끝 | 15.98M | 28k | 1,689,413 | 112.0GB |
| eh08 16M r1 | 시작 | 2.3M | 453k | 3,537 | 0 |
| | | 8.2M | 431k | 3,537 | 0 |
| | | 12.0M | 393k | 3,538 | 0 |
| | 끝 | 15.97M | 326k (평균) | 3,670 | 0 |
| eh04 16M r1 | 시작부터 벽 | 2.0M | 32k | 736,841 | 57.5GB |
| | | 4.0M | 31k | 1,461,282 | 122.2GB |
| | | 6.0M | 29k | 2,188,815 | 187.2GB |

- LH의 꺾임은 ~11–12M ops, majflt/read가 그 지점에서 폭발.
- EH08은 map이 캡보다 작아 majflt가 상수(3.5k) → 꺾임 없음.
- EH04는 시작부터 벽(map 845MB > 512MB): 첫 샘플부터 30k.

## (7) A1~A5 판정

| # | 기준 | 결과 | 근거 |
|---|---|---|---|
| A1 | EH(0.8) ≤ 0.6×LH(0.4) map | **조건부 통과** | production LH(0.4) 52.5 B/e 대비 0.539 / v4 실측 79.7 B/e 대비 0.355 / **in-harness lh(37.75) 대비 0.751 (미달)** |
| A2 | MEM 2G·N32M에서 EH 캡 내 & LH보다 높음 | **대체 통과, 원조건 미실행** | 512M·16M에서 EH08 452MB(캡 내) 325k vs LH 101k (3.2×). 2G·32M은 map이 캡 절반 이하라 벽이 아님 |
| A3 | hot EH(0.8) ≥ 0.5×LH | **실패** | 849k / 2,821k = **0.301** (swiss 대비 0.23) |
| A4 | placeFull==0, correctness 통과 | **통과** | P0 전부 PASS, 전 run placeFull 0 |
| A5 | EH dir bytes ≤ 5% of map | **통과** | 0.51% @1M, 0.50% @16M |

H 판정: **H1 지지(production LH 기준)**, **H2 지지(512M·16M)**, **H3 기각**.

## (8) 반증 / 미확인 / caveat

- **A3/H3 기각**: EH hot ops/s는 LH의 0.30×. 주 원인은 (a) EH의 pwrite write amplification
  (16M 삽입 write_bytes 26–49GB vs bench lh 0.3GB), (b) bench lh의 MAP_POPULATE + 직접 mmap store,
  (c) 20B slot. 디렉터리/lookup indirection 자체의 비용은 아님.
- **bench lh는 production v5.1이 아니다**(3.3): 8B slot·prefix 없음·verifyHash·load 0.5(presize 2N으로 실효 load 0.25).
  따라서 A1/H1/H2의 LH 수치는 "추세 참조"이며, production LH(0.4)(20B, load 0.4) 대비 계산을 병기했다.
  진짜 공정 비교는 LH v5.1(pwrite, prefix)을 같은 하니스로 돌리는 것 — 미실행.
- **EH_LOAD 0.7/0.8/0.9 수렴**: presize `2^(D-1)` 규칙 때문에 8M/16M에서 동일 구성. eh09는 eh08과
  map/splits/ops/s가 동일함을 1회 확인. 0.4만 별도(52.5 B/e).
- **P2 2G·N=32M 미실행**: 위 A2 참조. 512M·16M로 대체.
- **EH(0.4) 16M은 300s RuntimeMaxSec로 kill**(6.3M/16M). JSON 요약행 없음; 샘플만 보존.
- **swiss 2^32 미실행**: 55GB MAP_POPULATE라 P1 무캡에서는 불가. 기존 실측
  `l1-failpoint.jsonl: fp-p32-1680k` (N=1.68M, presize 2^32, MEM=2G): 2,137 ops/s,
  map 55.2GB, majflt 2.86M, read 56GB, 786s. 본 벤치에서는 인용만.
- EH08 16M 끝구간 rate 하락(453k→326k)은 map 452MB가 512MB 캡에 근접해 dirty writeback이
  조여지는 영향. majflt는 불변 → eviction은 아님.

## (9) 재현 명령

```bash
# 1) worktree
git -C /Volumes/mx500/utreexo-lab/bench-l1 worktree add ../bench-eh -b poc/eh-v1 aad6c84

# 2) VM (4GiB, 60GiB)
limactl start --name=eh-bench --vm-type=vz --cpus=4 --memory=4 --disk=60 --tty=false template://ubuntu-24.04

# 3) P0
cd /Volumes/mx500/utreexo-lab/bench-eh
bash scripts/run_eh_native.sh p0

# 4) P1 (swiss2N / lh / eh0.4 / eh0.8 at 1M,4M,16M, cap 3G)
bash scripts/run_eh_native.sh p1

# 5) P1b (lh, eh0.8; 4M/16M; 2:1,1:1)
bash scripts/run_eh_native.sh p1b

# 6) P2 (512M; 8M/16M; lh, eh0.4, eh0.8, eh0.9; 3회)
bash scripts/run_eh_native.sh p2a   # N=8M
bash scripts/run_eh_native.sh p2b   # N=16M
#   EH(0.4)-16M은 RuntimeMaxSec=300에서 kill되는 것이 정상(벽).

# 결과: results/eh-l1.jsonl, results/eh-wall.jsonl, results/eh-samples-*.jsonl
```

원시 수치: `results/eh-l1.jsonl`(20행), `results/eh-wall.jsonl`(16행), `results/eh-samples-*.jsonl`.

## (10) 다른 에이전트와의 충돌 여부

- 작업 디렉터리는 `bench-eh` worktree 하나. `bench-l1`은 읽기만, 커밋/리베이스 없음.
- VM은 `eh-bench`만 사용. `posmap-bench`는 애초에 존재하지 않았고 건드리지 않음.
- utreexod / docker compose `utreexo-lab` / `cw_utreexod_mainnet` / mx500 볼륨 마운트: 미접촉.
- VM RAM을 지시에 따라 8GiB→4GiB로 변경(host `~/.lima/eh-bench/lima.yaml`).
- 결과/코드 커밋은 `poc/eh-v1`에만. `poc/lh-*` 등 기존 브랜치 미변경.
- 11절 범위(prune flush, 정렬 read, prefix 8B, load 튜닝 등)는 시도하지 않았고 발견한 것 없음.
