# Mainnet baseline (LH v5.1) — before EH A/B

Captured read-only on 2026-10-19 ~17:49 KST (08:49 UTC). **No mainnet state was modified.**

| 항목 | 값 |
|---|---|
| container | `cw_utreexod_mainnet` |
| compose project label | `utreexo-lab` |
| image | `cw-utreexod-lh-v51:poc` |
| started | 2026-10-19T07:57:38Z (up ~52 min) |
| mode | `--proofonly --flatutreexoproofindex --prune=10000`, connect 192.168.0.55:8333 |
| env | `UTREEXOD_POSMAP=lh`, `GOMEMLIMIT=6GiB` |
| volume | `utreexod-mainnet-v3` (named, VM-local ext4) |
| mem_limit | 10g |

## Sporting metrics (read-only)
- height at capture: **426,258 → 426,362** (blocks ~2016-08), IBD in progress.
- **~1.4–1.5 blk/s** (logs: "Processed N blocks in ~10.2s", N≈9–29; periodic 47.8s flush pause).
- docker stats: CPU **104%**, mem **4.59 GiB / 10 GiB (45.9%)**, net 3.0 GB in / 14.8 MB out,
  block IO **82.7 GB read / 152 GB write**.
- connections: 1.

## LH_STATS (periodic)
```
ops=25165824 count=41136620 tomb=387138 numBuckets=804410 capBuckets=1048576
load=0.3995 usedRatio=0.4033 tombRatio=0.0093 splitsLoad=10983 splitsFull=0 placeFull=0
```
- posmap apparent ≈ 804,410 × 2688 B ≈ **2.16 GB** (cap 2^20 buckets = 2.82 GB), data cache ~759 MiB.

## EH deploy gate (blocked, needs explicit go)
Deploying EH is **not** a library-only change:
1. utreexod must be rebuilt/patched to expose `UTREEXOD_POSMAP=eh` → touches the utreexod repo
   and image (directive 2.2 forbids; image `cw-utreexod-lh-v51:poc` is shared).
2. EH file format ≠ LH → the existing position map is invalid; on restart the node rebuilds
   it from forest_data (at 41.1M live / height 426k, unknown duration) or needs fresh IBD.
3. The container is mid-IBD under project `utreexo-lab`; `docker-compose.v52.yml` and
   `docker-compose.yml` were modified at 16:28/17:36 today → another agent is active. Swapping
   the running node risks their run and the shared volume.
4. Earlier explicit instruction "메인넷은 건들지마" still stands.

Safe alternative: shadow container under a new project/name/volume (`utreexod-mainnet-eh`)
with EH wired in a scratch utreexod checkout, leave `cw_utreexod_mainnet` running. Compare on
the same height range. Needs either a volume copy or a fresh IBD.
