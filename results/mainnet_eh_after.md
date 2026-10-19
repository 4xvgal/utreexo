# Mainnet A/B: LH v5.1 → EH (in-place)

Captured 2026-10-19 ~18:15 KST (09:15 UTC). Same container/volume/ports; only the
image + `UTREEXOD_POSMAP` changed.

## What was deployed
- Branch: `utreexod:poc/eh-posmap` (from `poc/lh-posmap-v5`)
  - `utreexo-lh/internal/eh/*` (EHMap, reuse via consistency hash, max-depth guard)
  - `utreexo-lh/positionmap_eh.go` + `case "eh"` factory
  - `utreexo-lh/forest.go`: presize EH from **live** leaves (same as `lh`), not
    `expectedMaxLeaves=2^32`. (First attempt presized 90 GB; stopped before it filled the disk.)
- Image: `cw-utreexod-eh:poc` (`Dockerfile.lab`)
- Compose: `docker-compose.eh.yml` override; same volume `utreexod-mainnet-v3`.
- Container `cw_utreexod_mainnet`, `UTREEXOD_POSMAP=eh`, `UTREEXO_EH_LOAD=0.80`, `EH_STATS=1`.

## Rebuild cost (one-time, LH→EH)
- `Utreexo state loaded`: **09:06:11**, opened at 09:00:09 → **6m02s**.
- 41.9M live leaves inserted; `splits=260k`, `placeFull=0`.
- Extra block IO during rebuild: ~100–150 GB write (pwrite amp ~3.5 KB/entry).
- Sync resumed at height 426,937; missed blocks were then downloaded normally.

## Steady-state comparison (same chain region, 2016 blocks)

| metric | LH v5.1 (before) | EH (after) | delta |
|---|---|---|---|
| sync rate | **1.28–1.51 blk/s** (incl. 47.8s flush pause; 1.51 excl.) | **2.79 blk/s** (last 30 intervals; windows 2.43–3.24) | **~1.9–2.2× faster** |
| posmap buckets | 804,410 used / 1,048,576 cap | 523,218 | 1.54× fewer |
| posmap bytes | 2.16 GB used / **2.82 GB file** | **1.41 GB** (+4.5 MB dir) | **~2.0× smaller** |
| load | 0.3995 | 0.6199 (rising toward ~0.69) | +0.22 |
| tomb ratio | 0.93% | 2.17% (rising with churn) | +1.2pp |
| splits | 10,983 (load-driven) | 261,074 (overflow-only) | – |
| placeFull | 0 | 0 | = |
| cache | ~759 MiB | ~531 MiB (6 min after rebuild) | smaller |
| mem | 4.59 GiB / 10 GiB | 5.58 GiB / 10 GiB | +1 GiB |
| CPU | 104% | 86–103% | ≈ |
| block IO / block (window) | ~19 MB read / ~34 MB write (cum. estimate) | 11.9 MB read / 44.5 MB write | read ↓, write ↑ |
| errors | 0 | 0 | = |

Live height at capture: **428,065** (2016-09-03), 1 peer.

## Interpretation
- EH keeps the same 20 B prefix slot and pwrite path as LH v5.1, but at load 0.62 it
  needs **half the position-map file**, leaving more page cache for the forest/WAL and
  cutting the per-block read volume. Sync rate roughly doubled in this window.
- The gain here is **not** the cgroup memory wall (LH was at 4.6/10 GiB, not capped).
  It is the smaller map + fewer cache misses. At higher live counts EH's advantage
  grows (21 B/load vs 52.5 B/entry for LH at load 0.4).
- Trade-off: higher tomb ratio and more split work (overflow-driven); write IO per
  block is higher. Rebuild on format switch costs ~6 min + ~135 GB write once.

## Caveats
- Before/after windows are adjacent but not simultaneous; block sizes/tx counts differ.
- Baseline cumulative IO was converted with an estimated block count (approx).
- Cache was still warming at the EH capture (531 MiB vs LH's 759 MiB after 52 min).
- Other agents share this host; no control for their load.

## Rollback (keeps the EH files; LH map is untouched)
```bash
cd /Volumes/mx500/utreexo-lab
docker compose -p utreexo-lab -f docker-compose.yml -f docker-compose.v51.yml up -d
```
EH currently left running. Volume free space at capture: 40 GB; LH map (2.1 GB) retained for rollback.
