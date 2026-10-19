# EH position map 벤치 결과

## (1) 한 줄 결론

production LH v5.1과 **동일 조건(pwrite, 20B prefix slot)** 에서 EH(0.8)는 map이 **3.1× 작고**
hot ops/s가 **1.45~1.71× 빠르며**, 512M·16M 메모리 벽에서 **3.9× 빠르다** →
**EH는 LH v5.1을 대체할 수 있다.** 단 L2 end-to-end(P3)와 1:1 churn의 실제 forest 검증은 미실시.

## (2) 자원 / 커밋 / VM

- host worktree: `/Volumes/mx500/utreexo-lab/bench-eh` (branch `poc/eh-v1`, base `aad6c84`)
- 결과 JSONL `commit` 필드: 각 run 직전 HEAD (P1/P1b/P2 = `a23c5f8`, lh51 추가 run = `a23c5f8+`)
- VM: `eh-bench`, vz, 4 vCPU, **4GiB RAM**, 60GiB disk, Ubuntu 24.04 arm64 (kernel 6.8), Go 1.25.14
- cgroup: P1/P1b `MemoryMax=3G`, P2 `MemoryMax=512M`, 모두 `MemorySwapMax=0`.

## (3) P0 결과 (정확성/불변식)

`go test ./internal/eh` (N=1M, load 0.4/0.7/0.8/0.9 + eh8) — **전부 PASS**:
Count/Get hit·miss/절반 Delete/디렉터리 불변식(`localDepth<=D`, slot-prefix 일치)/ForEach==Count,
버킷당 ≤128, **`placeFull==0`**, eh8도 통과.

## (4) P1 표 (hot, cap 3G, mix=add, cold)

| N | impl | map (MB) | B/entry | ops/s | majflt |
|---|---|---|---|---|---|
| 1M | swiss(2N) | 25.71 | 25.71 | 3,735,831 | 0 |
| 1M | **lh51 (v5.1)** | **88.08** | **88.08** | **497,181** | 1 |
| 1M | lh (bench) | 37.75 | 37.75 | 2,821,490 | 0 |
| 1M | eh(0.4) | 53.27 | 53.27 | 1,063,717 | 519 |
| 1M | eh(0.8) | 28.36 | 28.36 | 848,791 | 237 |
| 4M | swiss(2N) | 102.86 | 25.71 | 2,863,528 | 0 |
| 4M | **lh51 (v5.1)** | **352.32** | **88.08** | **463,322** | 1 |
| 4M | lh (bench) | 151.00 | 37.75 | 1,962,883 | 0 |
| 4M | eh(0.4) | 211.70 | 52.92 | 832,562 | 2,134 |
| 4M | eh(0.8) | 113.42 | 28.36 | 802,140 | 939 |
| 16M | swiss(2N) | 411.43 | 25.71 | 1,030,221 | 0 |
| 16M | **lh51 (v5.1)** | **1409.29** | **88.08** | **362,909** | 1 |
| 16M | lh (bench) | 603.98 | 37.75 | 1,671,659 | 0 |
| 16M | eh(0.4) | 844.71 | 52.79 | 337,173 | 8,564 |
| 16M | eh(0.8) | 452.30 | 28.27 | 524,834 | 3,675 |

- lh51 map = 88.1 B/entry (pow2 presize + load 0.4 성장). v4 실측 79.7 B/entry와 같은 추세.
- **EH(0.8) / lh51**: map 0.322× (3.1× 작음), ops/s **1.71× / 1.73× / 1.45×** (N=1M/4M/16M).
- 실측 load: eh(0.4)=0.400(splits 0), eh(0.8)=0.747(splits 36,182 @16M), `placeFull=0`.
- dir bytes: 0.51% @1M, 0.50% @16M (A5).

### bench `lh`와 production `lh51`의 차이
bench lh는 8B slot·prefix 없음·verifyHash·**mmap 직접 store**·MAP_POPULATE·실효 load 0.25,
lh51은 20B prefix·**pwrite**·load 0.4·pow2 presize. 그래서 EH 대비 순위가 뒤집힌다:
- vs bench lh: EH hot 0.30× (A3 실패), map 0.75×.
- vs lh51: EH hot **1.45~1.71× 빠름** (A3 통과), map **0.32×** (A1 통과).
→ 앞선 A3 불통과는 EH 주소지정이 아니라 harness의 write path/슬롯 차이였다.

## (5) P1b 표 (churn, cap 3G)

| N | mix | impl | map (MB) | B/entry | ops/s | tombRatio | splits |
|---|---|---|---|---|---|---|---|
| 4M | 2:1 | lh51 | 176.22 | 44.0 | 691,862 | – | 0 |
| 4M | 2:1 | eh(0.8) | 88.64 | 22.16 | 931,136 | 1.6% | **0** |
| 16M | 2:1 | lh51 | 704.64 | 44.0 | 568,654 | – | 0 |
| 16M | 2:1 | eh(0.8) | 354.55 | 22.16 | 649,741 | 1.6% | **0** |
| 4M | 2:1 | lh (bench) | 151.00 | 37.75 | 2,395,160 | – | 0 |
| 4M | 1:1 | lh (bench) | 151.00 | 37.75 | 8,019,496 | – | 0 |
| 4M | 1:1 | eh(0.8) | 88.64 | 22.16 | 1,077,362 | – | **0** |
| 16M | 1:1 | lh (bench) | 603.98 | 37.75 | 7,268,099 | – | 0 |
| 16M | 1:1 | eh(0.8) | 354.55 | 22.16 | 823,131 | – | **0** |

- EH split 0회: tombstone이 overflow/cascade를 만들지 않음.
- 2:1에서 EH08은 lh51보다 1.14~1.35× 빠르고 map 2× 작음.
- 1:1은 l1GenOps 특성상 live가 작아 EH presize가 과대(공정 steady-state는 미실행).

## (6) P2: 메모리 벽 (MEM=512M, mix=add, 스트리밍)

### 6a. run 요약 (3회 중앙값; EH04-16M은 kill)

| N | impl | map (MB) | ops/s (med) | cgroup peak | majflt | read (GB) | 완료 |
|---|---|---|---|---|---|---|---|
| 8M | eh(0.8) | 226.2 | **444,524** | 230 | 1,843 | 0 | ✅ |
| 8M | eh(0.4) | 422.7 | 335,673 | 422 | 4,223 | 0 | ✅ |
| 8M | **lh51** | **704.6** | **322,493** | 417 | 1.5 | 0 | ✅ |
| 8M | lh (bench) | 302.0 | 1,031,505 | 512 | 3,270 | 0.28 | ✅ |
| 16M | eh(0.8) | 452.3 | **325,163** | 453 | 3,671 | 0 | ✅ |
| 16M | **lh51** | **1409.3** | **83,414** | 512 | 1,419,000 | 104.0 | ✅ (189–195s) |
| 16M | lh (bench) | 604.0 | 100,604 | 512 | 1,692,559 | 111.6 | ✅ |
| 16M | eh(0.4) | 844.7 | ~20,000 | 512 | 2,280,383 | 195.3 | ❌ 6.3M/16M kill |

- EH08은 캡 내(452<512) → majflt 3.7k, read 0, 무꺾임.
- lh51 16M은 map 1.34GB > 캡 → read 104GB, majflt 1.42M, 84k ops/s. **EH08이 3.9× 빠름.**
- lh51 8M은 map 704MB로 캡 초과지만 322k 유지(도중 336→672MB doubling).
- 8M에서 bench lh(1.03M)가 가장 빠른 이유는 mmap store·8B slot·실효 load 0.25의 harness 이점.
  production lh51과 비교하면 EH08이 오히려 1.38× 빠르다.

### 6b. 꺾임 지점 (per-second 샘플)

| run | ops | ops/s | majflt | read |
|---|---|---|---|---|
| lh 16M (bench) | 4.5M | 2,002k | 950 | 0.1GB |
| | 10.4M | 553k | 3,372 | 0.3GB |
| | **12.0M (꺾임)** | **15k** | 271,145 | 11.3GB |
| | 15.98M | 28k | 1,689,413 | 112.0GB |
| lh51 16M | 12.6M | 163k | 394,877 | 21.8GB |
| | 14.7M | 108k | 969,597 | 62.7GB |
| | 15.73M | 88k | 1,318,330 | 91.0GB |
| eh08 16M | 8.2M | 431k | 3,537 | 0 |
| | 12.0M | 393k | 3,538 | 0 |
| | 15.97M | 326k (평균) | 3,670 | 0 |
| eh04 16M | 2.0M | 32k | 736,841 | 57.5GB |
| | 6.0M (kill) | 29k | 2,188,815 | 187.2GB |

- lh51은 12.6M ops부터 서서히 꺾여 88k까지 하락; EH08은 끝까지 326k, majflt 불변.

## (7) A1~A5 판정

| # | 기준 | 결과 | 근거 |
|---|---|---|---|
| A1 | EH(0.8) ≤ 0.6×LH(0.4) map | **통과 (production LH 기준)** | vs lh51 88.1 B/e → **0.322**; vs v4 실측 79.7 → 0.355; (bench lh 37.75 → 0.751, harness artifact) |
| A2 | 캡 초과 구간에서 EH 캡 내 & LH보다 높음 | **통과 (512M·16M)** | EH08 452MB·325k vs lh51 1409MB·83k = **3.9×**. (2G·32M은 벽 아님, 미실행) |
| A3 | hot EH(0.8) ≥ 0.5×LH | **통과 (production LH 기준)** | vs lh51: 1.71/1.73/1.45×; (bench lh 대비 0.30×는 write-path 차이) |
| A4 | placeFull==0, correctness | **통과** | P0 PASS, 전 run placeFull 0 |
| A5 | EH dir bytes ≤5% map | **통과** | 0.50~0.51% |

**H1 지지, H2 지지, H3 지지(production LH 기준).** bench lh 기준 H3만 기각이었고, 그 원인은 분리됨.

## (8) 반증 / 미확인 / caveat

- **lh51은 test package 포트**: production 타입은 package `utreexo`에 있어 `internal/swisstable`로
  import할 수 없다(순환). v5.1의 상수/흐름(2688B bucket, 20B slot, load 0.4, prefix lookup, pwrite,
  pow2 doubling)을 그대로 옮겼다. 리터럴 동일 타입은 아님.
- **P3(L2 end-to-end) 미실행**: 실 Forest의 `ModifyAndReturnTTLs` 경로 미검증.
- **1:1 churn**: harness 특성상 live가 작아 EH presize 과대. steady-state presize/live 기준 재측정 필요.
- **EH_LOAD 0.7/0.8/0.9 수렴**: presize `2^(D-1)` 규칙상 8M/16M에서 동일(eh09=eh08 확인). 0.4만 별도.
- **EH(0.4) 16M @512M은 kill**: 샘플만 보존(요약행 없음).
- **swiss 2^32 미실행**: 55GB MAP_POPULATE. 기존 `fp-p32-1680k`(N=1.68M, 2G): 2,137 ops/s, majflt 2.86M, read 56GB, 786s.
- EH pwrite write amp는 캡 내에서 page cache reclaim으로 흡수되지만, write_bytes는 lh51(16M 삽입 64GB)과
  EH08(26GB) 모두 크다. hot 차이의 주 원인은 여전히 write path 대비가 아니라 load/slot 밀도 차이.

## (9) 재현 명령

```bash
git -C /Volumes/mx500/utreexo-lab/bench-l1 worktree add ../bench-eh -b poc/eh-v1 aad6c84
limactl start --name=eh-bench --vm-type=vz --cpus=4 --memory=4 --disk=60 --tty=false template://ubuntu-24.04
cd /Volumes/mx500/utreexo-lab/bench-eh
bash scripts/run_eh_native.sh p0
bash scripts/run_eh_native.sh p1
bash scripts/run_eh_native.sh p1b
bash scripts/run_eh_native.sh p1lh51      # production LH v5.1 포트, hot
bash scripts/run_eh_native.sh p2a
bash scripts/run_eh_native.sh p2b
bash scripts/run_eh_native.sh p2lh51      # production LH v5.1 포트, 512M 벽
# 결과: results/eh-l1.jsonl, results/eh-wall.jsonl, results/eh-samples-*.jsonl
```

## (10) 다른 에이전트와의 충돌 여부

- `bench-eh` worktree만 수정. `bench-l1`은 읽기만. 기존 `poc/lh-*` 브랜치 미변경.
- VM `eh-bench`만 사용(`posmap-bench`는 존재하지 않음). RAM 8→4GiB 변경.
- utreexod / docker compose `utreexo-lab` / `cw_utreexod_mainnet` / mx500 볼륨 마운트: 미접촉.
- 범위 밖 11절 항목은 시도하지 않음.
