# LH v4 (live-only position map) — results

Date: 2026-10-19
Branches: `bench-l1` `poc/lh-liveonly`, `utreexod` `poc/lh-posmap-v4`
Image: `cw-utreexod-lh-v4:poc` (posmap magic `LH4`, undo record `UTR4`)

## Goal

Make the position map hold only live UTXO leaves, not every leaf ever created,
so the mainnet bridge's working set drops from 15.3 GB to ~2.6 GB and the map
fits in RAM. v3 (`LH3`) kept spent entries, so `count == forest_meta.numLeaves`
(291,383,891) and the table was presized to 2^23 buckets (22.5 GB file).

## Changes

Library (`bench-l1`, branch `poc/lh-liveonly`):

- `deleteSingle` deletes the spent leaf's map entry after tree surgery.
- `processDeletions` (Record / proof-pipeline path) deletes too, so IBD under
  the pipeline is live-only.
- `ModifyAndReturnTTLs` also returns the deleted leaf positions.
- `Undo` re-inserts spent leaves from `Proof.DelPacked` (packed position+add
  index), carried in the undo record; `Proof.Targets` is sorted and cannot
  recover the mapping.
- `rebuildPositionMap` skips `deletedLeafPositions`.
- Rebuild presizes the LH table to the **live** count
  (`NumLeaves - deletedLeafPositions.count()`) instead of `NumLeaves`.
  Without this the rebuild re-creates the 2^23-bucket table.
- LH magic `LH3` → `LH4` forces the one-time rebuild.
- `Prove` fills `DelPacked` for callers that save the proof for undo.

Node (`utreexod`, branch `poc/lh-posmap-v4`):

- Undo record v2 (`UTR4`) appends the packed deletion positions.
- Undo blocks are written for every node (pruned and archive), since the
  stored proof serialization does not carry `DelPacked`.
- Flat and DB indices recover `DelPacked` when undoing.
- Regtest stack: `poc/docker-compose.regtest-v4.yml`.

## Phase A — library tests

`go test ./...` (golang:1.25-bookworm): all pass. The `sanityCheck` expectation
was updated to `NumLeaves - deleted`, and tests that undo through the map got
`DelPacked` from `Prove`/`ModifyAndReturnTTLs`.

`TestLHPositionMapKeyPrefix` covers N insert → delete half → `Count()==N/2`,
deleted keys miss, live keys hit, reopen reuses the table with the surviving
count, and a different consistency hash forces a rebuild.

## Phase B — regtest

`poc/docker-compose.regtest-v4.yml` (bitcoind + utreexod
`--flatutreexoproofindex --proofonly --prune=550`, `UTREEXOD_POSMAP=lh`) with 200+
blocks, spends, and a forced reorg (`invalidateblock` + 2 competing blocks):

- magic `LH4`, `placeFull=0`, no panic.
- Reorg logged `REORGANIZE` and completed with no
  `undo: N delHashes but 0 packed positions`.
- `LH_STATS close`: `count=208 tomb=2 ... placeFull=0` — count tracks live, not
  cumulative.
- utreexod wipes the regtest datadir on every start, so restart persistence is
  covered by the unit tests, not regtest.

## Phase C — mainnet remeasure

Graceful stop of `cw_utreexod_mainnet` (v3), then start on the same
`utreexod-mainnet-v3` volume with `cw-utreexod-lh-v4:poc`.

### Before (v3, H=404,179)

```
posmap magic=LH3 level=22 split=1,496,788 count=291,383,891
file=22.5 GB  capBuckets=8,388,608
forest_deleted.dat popcount: deleted=256,316,172  live=35,062,516
majflt ~5,600/s
~1.1–1.3 blk/s
```

### Rebuild (v4 open)

```
LH_STATS open reuse=false count=0 numBuckets=1 capBuckets=1048576
             expectedEntries=35444175
placeFull=0 through the whole rebuild
```

Rebuild completed in a few minutes. `expectedEntries` now uses the live count,
so `capBuckets=2^20` instead of `2^23`.

### After (v4, H≈404,400–404,670)

```
posmap file=2.82 GB  (2,818,572,352 B, capBuckets=1,048,576)
LH_STATS periodic count≈33.5M…35.4M  placeFull=0
majflt: ~7/5 s when idle, ~3,700/s during active IBD (was ~5,600/s)
throughput: ~100 blocks/min over 60 s (~1.7 blk/s), peaks to 2.3 blk/s
            (pre-swap ~1.1–1.3 blk/s)
```

The map shrank **8.0×** (22.5 GB → 2.82 GB) and the count is live-only. The
major-fault rate fell, but not to zero: during IBD the node still faults on
`forest_data.dat` (~17 GB used) and the 70 GB `blocks_ffldb`, which the 10 GB
cgroup limit cannot cache.

## Conclusion

The v4 target — a live-only position map that fits in RAM — is met: 22.5 GB →
2.82 GB, `placeFull=0`, live count, and undo works through reorgs on both the
pruned and archive paths.

The predicted large throughput jump did **not** materialize. Removing the map
as a bottleneck reveals the next one: block-file and `forest_data.dat` I/O
(`read_bytes` ~26 MB/s from disk, `rchar` ~135 MB/s, ~1 core CPU). The 10 GB
cgroup limit is now the constraint, not the map. Next step is profiling those
paths (prune target / block file cache, proof-flat I/O) rather than the map.

## Rollback

Graceful stop, then start `cw-utreexod-lh-v3:poc`; the `LH4` magic mismatch
triggers a rebuild. Do not delete `forest_data.dat`.

## Known limitations

- `pprof`/crash during IBD before the next utreexo-state flush would leave the
  map header hash unset and force another rebuild; a graceful stop flushes.
- The archive→pruned transition (`pruneProofs`) no longer regenerates undo
  blocks from proofs (that dropped `DelPacked`); it relies on the blocks written
  at connect. A v3 archive node upgrading to v4 has v1 undo records without
  `DelPacked`, so a deep reorg would need a reindex.
- The DB (`UtreexoProofIndex`) prune transition has the same v3-archive caveat.
