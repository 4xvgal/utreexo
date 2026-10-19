# EH bench plan (요약: 지시문 4~6절)

## 4. EH 설계 (구현 확정)
- bucket: `ctrl[128] + slot[128]*20B = 2688B` (packed u64 + 12B prefix), LH v5.1과 동일.
- dir: `header 64B + dirSlot[2^D]*8B + localDepth[bucketCount]` (depth 배열은 뒤).
- header: magic `EH01`, D, bucketCount, count, consistencyHash(32B).
- hash: h1=LE(hash[0:8]), h2=(hash[8]&0x7f)|0x80, prefix=hash[0:12].
- directory index = `h1 >> (64-D)` (MSB). split은 bucket full일 때만.
  - d<D: 새 버킷 B', d++, bit `(h1>>(63-d))&1`로 재배치, alias 슬롯 upper half 갱신.
  - d==D: D++ 후 위와 동일.
  - 재배치는 저장된 prefix의 h1으로 수행 → 데이터 파일 접근 없음.
- delete: tombstone `ctrl=0x7F`, slot zero, count--. merge/축소 없음.
- 읽기: read-only `MAP_SHARED` mmap. 쓰기: `pwrite` (v5.1 규약).
- EH_LOAD: presize 목표. split 트리거는 아님(관측값).
- eh8(옵션): 8B slot + verifyHash. `BENCH_IMPL=eh8`.

### 지시문 대비 deviation
1. **directory doubling**: `old[i] → new[2i], new[2i+1]` (interleave).
   `h1>>(64-D)` MSB 인덱스에서는 새 비트가 인덱스 LSB로 붙으므로
   지시문의 `new[i], new[i+2^D]`는 성립하지 않는다. 4.2/6.1 불변식을 위해 interleave.
2. **presize**: load<=0.5는 mixed-depth로 목표 버킷 수를 정확히 맞춤.
   load>0.5는 `2^(D-1)`개를 모두 local depth D-1로 깔고 full-split에 맡김.
   이유: depth D-1 버킷은 디렉터리 슬롯 2개 alias라 키 유입이 2배 →
   mixed-depth를 0.8에 쓰면 불필요 split이 폭증(1M에서 1,803회, map 31.8MB vs 28.4MB).
   결과적으로 EH_LOAD 0.7/0.8/0.9는 같은 초기 크기로 수렴하고, 실측 load는 ~0.75.
3. **eh8**: prefix 없는 변형도 구현(원인 분해용). P0 통과, P1/P2는 prefix 판만 측정.

## 5. 하니스 통합 (구현 확정)
- `internal/eh/eh.go`: `NewEHMap(dir, load, prefix)`, `Presize(n)`,
  `Set/Get/Delete/Count/ForEach/NumSlots/MapBytes/DropCache/Close`,
  `EH_STATS`(기본 2^22 ops, `EH_STAT_EVERY`) + `EH_STATS=0` off.
- `internal/swisstable/bench_l1_eh_test.go`:
  `l1EHMap` 어댑터, `l1NewEH(dir, expectedEntries, src, posMask)`,
  `BenchmarkL1Wall`(스트리밍 키 생성, P2용), per-op `Ops()` 카운터,
  5초 watchdog(`BENCH_PROGRESS`), 1M ops `WALL_PROGRESS`, 1초 샘플러(`EH_SAMPLES`).
- `bench_l1_alt_test.go`: `case "eh", "eh8"` 2줄만 추가.
- `scripts/run_eh_native.sh`: VM=eh-bench, guest `$HOME/eh-bench`,
  `systemd-run --property=MemoryMax --property=MemorySwapMax=0`,
  `RuntimeMaxSec`로 행 방지, commit 스탬프.

## 6. 벤치 프로토콜 (실행 확정)
- P0: N=1M, load 0.4/0.7/0.8/0.9 + eh8. 정확성/디렉터리 불변식/placeFull.
- P1: N=1M/4M/16M, impl swiss(2N), lh, eh(0.4), eh(0.8), mix=add, cap 3G.
- P1b: N=4M/16M, mix 2:1/1:1, impl lh, eh(0.8), cap 3G.
- P2: MEM=512M, N=8M/16M, impl lh/eh(0.4)/eh(0.8)/eh(0.9), 3회, 중앙값.
  - 기존 하니스는 keys(N*32B)+ops trace가 캡에 안 들어가므로 스트리밍 `BenchmarkL1Wall` 사용.
  - 지시문의 MEM=2G·N=32M은 map이 캡의 절반 이하라 벽이 아니어서 512M·16M로 대체.
  - swiss 2^32(55GB)는 실행하지 않고 기존 failpoint 실측을 인용.
