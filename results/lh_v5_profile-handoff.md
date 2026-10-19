# 지시문: LH v4 이후 병목 프로파일링 (v5)

대상: 새로 초기화된 에이전트. 이 문서만으로 프로파일링 → 원인 확정 → 다음 수정 방향
결정까지. 선행 결과는 아래 "2. 이미 확인된 사실"에 전부 수치로 박아 둔다(다른 문서 안
봐도 됨).

목표 산출물: `lh_v5_profile_report.md` — 병목을 증거로 특정하고, 가장 큰 레버리지
수정 1개와 기대 이득을 제시.

────────────────────────────────────────────────────────────────────────────────

0. 한 줄 목표

v4(live-only posmap)는 map을 22.5GB→2.82GB로 줄였고 majflt도 낮췄지만 IBD 처리량은
~1.7–2.8 blk/s에 머문다. 이제 진짜 병목이 무엇인지(블록파일 I/O / forest_data I/O /
proof 쓰기 / 단일 코어 직렬화 / 10GB cgroup) 증거로 확정한다.

────────────────────────────────────────────────────────────────────────────────

1. 현재 실행 상태 (건드리지 말 것)

- 컨테이너: `cw_utreexod_mainnet` (OrbStack), 이미지 `cw-utreexod-lh-v4:poc`
- 노드 브랜치: `utreexod` `poc/lh-posmap-v4`
- 실행 정의:
  - `/Volumes/mx500/utreexo-lab/docker-compose.yml` (base, image v3로 적혀 있음)
  - `/Volumes/mx500/utreexo-lab/docker-compose.v4.yml` (override: image v4, POSMAP_STATS=1)
  - project name `utreexo-lab`
- 데이터 볼륨: `utreexod-mainnet-v3` (v4가 그대로 재사용)
- 데이터 경로(컨테이너 내부): `/data/mainnet/`
- 현재 high: H≈404,750 (2026-10-19 03:36), 계속 증가 중
- 기동 명령:
  `cd /Volumes/mx500/utreexo-lab && docker compose -p utreexo-lab -f docker-compose.yml -f docker-compose.v4.yml up -d`
- 정지(반드시 graceful): `... stop`
- 이 노드는 지금 IBD를 계속 돌리고 있다. 프로파일링을 위해 재시작할 때도 graceful만.

주요 플래그(compose command):
`--flatutreexoproofindex --proofonly --prune=5000 --utxocachemaxsize=1024
--utreexoproofindexmaxmemory=1024 --connect=${BTC_FULLNODE:-192.168.0.55:8333}`
환경변수: `UTREEXOD_POSMAP=lh`, `UTREEXOD_POSMAP_STATS=1`, `UTREEXOD_MAXBLKFILE=16777216`,
`mem_limit: 10g`, `stop_grace_period: 10m`.

────────────────────────────────────────────────────────────────────────────────

2. 이미 확인된 사실 (v4 측정, 2026-10-19)

posmap (v4, 정상):
- magic `LH4`, level=20, capBuckets=1,048,576, file=2,818,572,352 B (2.82GB)
- LH_STATS periodic: count≈33.5M→35.4M, tomb=0, load≈0.40, `placeFull=0`
- `expectedEntries=35,444,175` (= NumLeaves 291,383,891 − deleted 256,316,172)
- rebuild만 수 분, 이후 실패/panic 없음
- v3 대비: file 22.5GB→2.82GB (8.0×), majflt ~5,600/s→약 3,700/s(IBD중)

디스크(컨테이너 내부):
- `blocks_ffldb` = 70 GB   ← prune=5000(5GB)인데 70GB
- `proof_flat`    = 47 GB
- `leafdata_flat` = 3.3 GB
- `target_flat`   = 1.8 GB
- `ttls_flat`     = 2.3 GB
- `roots_flat`    = 154 MB
- `undo_flat`     = 170 MB
- `utreexostate_flat` = 20 GB (그중 forest_data.dat는 sparse 1,099,511,628,672 B,
  실제 사용 ~17GB, forest_posmap_ctrl.dat 2.82GB)

/proc/1/io 델타 (10초, IBD 중):
- `read_bytes` ≈ 26 MB/s, `rchar` ≈ 135 MB/s (대부분 page cache hit)
- `write_bytes` ≈ 215 MB/s, `wchar` ≈ 3.9 MB/s   ← write가 비정상적으로 큼
- 누적: read_bytes 30GB, write_bytes 1.21 TB

/proc/1/stat 델타 (10초, IBD 중):
- utime=295, stime=730 (커널 시간이 70%)
- minflt ≈ 260k/10s (26k/s), majflt ≈ 37.5k/10s (3.7k/s)
- CPU: `docker stats` ≈ 105% (≈1 코어)

메모리:
- cgroup `memory.current` = 7.6 GB / `memory.max` = 10 GB (10,737,418,240)
- `/proc/meminfo`: MemAvailable 4.3GB, Cached 4.2GB, MemFree 126MB

처리량(관찰, 변동 큼):
- v3 stop 직전 H=404,179: 11–13 blk/10s (~1.1–1.3 blk/s)
- v4 H≈404,400–404,750: 7–30 blk/10s, 60초 구간 101 blk/min (~1.7 blk/s),
  순간 peak ~2.8 blk/s
- tx 기준으로도 v3 ~10–14k tx/10s, v4 ~7–31k tx/10s 로 큰 차이 없음

코드/아키텍처 사실:
- IBD 중 `--flatutreexoproofindex`는 proof pipeline 4단계
  (preprocess → Record → RehashAndProve(generate) → store)를 돌린다.
- `--proofonly`는 config에서 `KeepUtreexoProofs=true`를 강제한다(config.go: `if cfg.ProofOnly { cfg.KeepUtreexoProofs = true }`).
- `--prune=5000`이면 `PruneBlock` → `dbTx.PruneBlocks(pruneTarget, tip-287)`가
  호출되는 경로가 있다(blockchain/chain.go connectBlock 안).
- forest는 row-based flat file. `posToFileOffset`로 행별 dense 배치. deleteSingle과
  RehashAndProve가 부모/형제를 readHash로 랜덤하게 읽는다.

────────────────────────────────────────────────────────────────────────────────

3. 가설 (우선순위)

H1 (강): blocks_ffldb 70GB가 prune되지 않음. `--proofonly`/KeepUtreexoProofs가 블록 prune을
  막거나, prune 트리거가 tip 연결 시에만 돌아 backlog를 못 따라감. 70GB를 매 블록 read하면
  막대한 major fault. → prune 실동작 감사.

H2 (강): `write_bytes` 215MB/s의 정체. 후보: (a) proof/leafdata/target/ttls store의
  append write, (b) forest_data.dat sparse 확장/페이지폴트 write, (c) WAL. 이 write가
  read 대역과 경쟁.

H3 (중): forest_data.dat(실사용 ~17GB) 랜덤 read. cgroup 10GB에서 block files와 서로
  cache를 밀어냄. mapping별 Rss/fault로 어느 파일이 fault를 만드는지 확인.

H4 (중): 단일 코어 직렬화. CPU 105% + stime 70%는 user CPU 포화가 아니라 syscall/mmap
  대기. proof pipeline 단계 중 하나가 lock/SQL/파일 flush로 직렬화되면 처리량이 1코어로
  캡됨.

H5 (약): 피어(`--connect`) 공급 속도 한계. 로컬 full node가 그 속도로만 블록을 줌.
  net rx로 확인.

H6 (약): 10GB cgroup 자체가 상한. page cache가 회수돼 forest/block이 thrash.

────────────────────────────────────────────────────────────────────────────────

4. 절대 규칙

1. 컨테이너 종료는 graceful만: `docker compose ... stop`. `docker rm -f`/`kill -9`/
   `compose down -v` 금지.
2. `forest_data.dat` 삭제 금지 (rebuild 원천).
3. 프로파일링 재시작도 graceful. 재시작 후 magic LH4/posmap 재사용 여부 확인(재build면
   consistency hash 미기록 상태였다는 뜻).
4. 코드 수정이 필요하면 별도 브랜치, regtest 통과 후에만 메인넷 적용.
5. 메인넷 볼륨은 `utreexod-mainnet-v3`. 새 볼륨 만들지 말 것.

────────────────────────────────────────────────────────────────────────────────

5. 계측 명령 (복붙용)

기본 채집(컨테이너 안, debian-slim이라 curl/wget 없음 → sh+awk만):
```
# stat: utime stime minflt majflt (10s 델타)
docker exec cw_utreexod_mainnet sh -c '
  awk "{print \$14,\$15,\$10,\$12}" /proc/1/stat > /tmp/a; sleep 10;
  awk "{print \$14,\$15,\$10,\$12}" /proc/1/stat > /tmp/b;
  paste /tmp/a /tmp/b | awk "{printf \"utime=%d stime=%d minflt=%d majflt=%d /10s\n\", \$5-\$1,\$6-\$2,\$7-\$3,\$8-\$4}"'

# io: rchar wchar read_bytes write_bytes (10s 델타)
docker exec cw_utreexod_mainnet sh -c '
  cat /proc/1/io > /tmp/io1; sleep 10; cat /proc/1/io > /tmp/io2;
  paste /tmp/io1 /tmp/io2 | awk "{printf \"%s delta=%d\n\", \$1, \$4-\$2}"'

# 메모리/캐시
docker exec cw_utreexod_mainnet sh -c 'grep -E "MemFree|MemAvailable|Cached|Dirty|Writeback" /proc/meminfo'
docker exec cw_utreexod_mainnet sh -c 'cat /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.max'
docker stats --no-stream cw_utreexod_mainnet

# mapping별 Rss (어느 파일이 RAM을 먹는지)
docker exec cw_utreexod_mainnet sh -c 'cat /proc/1/smaps_rollup; grep -B1 "^Rss:" /proc/1/smaps | grep -A1 "forest_data\|blocks\|proof_flat" | head'
```

pprof (재시작 1회 필요, graceful):
- override에 `--profile=6060` 추가(env 아님 command 인자). 포트 publish 불필요.
- 같은 netns로 golang 컨테이너를 붙여 원격 수집:
```
docker run --rm --net container:cw_utreexod_mainnet -v "$PWD/prof":/prof \
  -w /prof golang:1.25-bookworm sh -c '
    go tool pprof -top -cum -seconds=60 -output=/prof/cpu.txt http://127.0.0.1:6060/debug/pprof/profile
    go tool pprof -top -cum -output=/prof/heap.txt http://127.0.0.1:6060/debug/pprof/heap
    go tool pprof -top -cum -output=/prof/goroutine.txt http://127.0.0.1:6060/debug/pprof/goroutine'
```
- block/mutex profile은 기본 rate=0이라 비어 있다. 필요하면 재시작 시
  `block`/`mutex` rate를 켜는 코드를 임시 브랜치에 넣고 regtest로만 검증(메인넷 직접 X).
  대안: `--traceprofile=/data/trace.out`로 runtime trace를 짧게 받아 `go tool trace`.

prune 감사:
```
# 10분간 blocks_ffldb 증가량
docker exec cw_utreexod_mainnet sh -c 'du -sb /data/mainnet/blocks_ffldb; sleep 600; du -sb /data/mainnet/blocks_ffldb'
# prune 로그
docker logs cw_utreexod_mainnet 2>&1 | grep -iE "prune|Pruning|PruneBlock" | tail
```
코드 읽을 곳: `blockchain/chain.go` connectBlock의 `dbTx.PruneBlocks` 호출부,
`database/ffldb`의 `PruneBlocks`, `blockchain/indexers/flatutreexoproofindex.go`의
`PruneBlock`.

proof 쓰기 감사:
```
docker exec cw_utreexod_mainnet sh -c 'du -sb /data/mainnet/proof_flat /data/mainnet/leafdata_flat /data/mainnet/target_flat /data/mainnet/ttls_flat; sleep 600; du -sb /data/mainnet/proof_flat'
```

네트워크 유입(피어 공급):
```
docker exec cw_utreexod_mainnet sh -c 'cat /proc/net/dev'   # 10s 간격 2회 델타
```

────────────────────────────────────────────────────────────────────────────────

6. 실험 (E1–E6)

E1. 기준선 확보
- 5–10분 동안 10초 간격으로 stat/io/mem/height/tx 샘플을 CSV로. blk/s가 아니라 tx/s와
  바이트/s를 기준으로. 높이 구간별 tx 밀도가 달라 blk/s는 노이즈가 큼.
- 산출: `baseline.csv`.

E2. CPU attribution
- pprof cpu 60초 top/cum. top에 `syscall.*`/`runtime.mmap`/`os.File.ReadAt`/`unix.pread`
  vs `utreexo.(*Forest)...`/`parentHash` 중 무엇이 상위인지.
- 해석: syscall/mmap 우세면 H4/H3(IO 직렬화), utreexo 함수 우세면 H4(CPU 직렬화).

E3. Fault attribution
- `/proc/1/smaps`에서 파일별 Rss와 (가능하면) fault. `forest_data.dat`, `blocks_ffldb`,
  `proof_flat`, `forest_posmap_ctrl.dat` 중 누가 fault를 만드는지.
- 별도로 `minor`/`major` fault가 어느 시점(블록 connect vs flush vs proof store)에
  튀는지 E1 CSV와 상관.

E4. Prune 실동작
- blocks_ffldb가 증가하면 H1 확정. `--proofonly`가 block prune을 건너뛰는지 코드 확인.
- 실험: regtest에서 `--flatutreexoproofindex --prune=550`(proofonly 없이)로 550MiB를
  넘겨 prune이 동작하는지. 메인넷 이미지 그대로, 새 볼륨에서. (메인넷 볼륨 X)
- 기대: prune이 살아나면 70GB→수 GB, block read fault 급감.

E5. proof retention 비용
- proof_flat 증가율 측정. `--proofonly`가 KeepUtreexoProofs를 강제하므로 브릿지 요구사항.
- 실험(regtest/로컬): KeepUtreexoProofs=false(프룬, proof 미보관) 대비 처리량 차이.
  차이가 크면 "bridge는 proof를 다 보관" 정책 자체가 병목.

E6. 캐시/레이아웃 토글 (lab only)
- `--utxocachemaxsize`/`--utreexoproofindexmaxmemory`를 낮춰 block cache에 RAM을
  넘겨주면 fault가 줄고 blk/s가 오르는지.
- `mem_limit` 10g→14g(OrbStack 호스트 허용 시)로 page cache 압박이 풀리는지.
- forest_data.dat를 `mmap`+`MADV_RANDOM`/`posix_fadvise` 또는 readahead로 바꾸면 forest
  fault가 줄는지(코드 실험은 브랜치+regtest).

────────────────────────────────────────────────────────────────────────────────

7. 판정 기준

성공:
- wall time에서 차지하는 비중으로 병목 순위를 제시: 예) block file read 45%, proof write
  25%, forest read 20%, CPU 10%. 각 수치에 근거 명령/로그 첨부.
- "가장 큰 레버 1개 + 기대 배수"를 명시. 예: prune 복구로 blocks_ffldb 70GB→5GB,
  예상 처리량 ×N. 근거는 E4의 regtest 결과.

실패(허용): 원인이 단일하지 않으면 상위 3개와 각각의 증거, 그리고 추가 계측이 필요한
지점을 명시.

────────────────────────────────────────────────────────────────────────────────

8. 롤백 / 안전

- v4 문제 시: graceful stop → `cw-utreexod-lh-v3:poc` 기동. LH4 magic mismatch로 rebuild.
- 프로파일링용 `--profile` 인자는 성능 오버헤드가 있으니 CPU 프로파일은 60초만, 끝나면
  원복.
- write 폭주(215MB/s)가 디스크 수명/용량에 영향을 줄 수 있으니 E1 관찰 중 `df -h /data`도
  기록.

────────────────────────────────────────────────────────────────────────────────

9. 참고 코드 위치

- forest I/O/삭제/재해시: `bench-l1/forest.go`의 `deleteSingle`, `rehashToRoot`,
  `readHash`/`writeHash`, `posToFileOffset`; `forest_roots.go`의 `RehashAndProve`,
  `rehashDeletionRow`.
- live-only map: `bench-l1/positionmap_lh.go`의 `insert/split/Delete`.
- node proof pipeline / store: `utreexod/blockchain/indexers/flatutreexoproofindex.go`의
  `preprocess`, `record`, `generate`, `store`, `storeProof`, `storeUndoBlock`.
- prune: `blockchain/chain.go` connectBlock의 `PruneBlocks` 호출, `database/ffldb`,
  `flatutreexoproofindex.go`의 `PruneBlock`.
- config: `utreexod/config.go`의 Profile/CPUProfile/ProofOnly/Prune/KeepUtreexoProofs.
