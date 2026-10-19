//go:build linux

// L2 benchmark: the utreexo Forest end-to-end (position map + forest_data mmap
// + verifyHash + f.mu), driven through the public API. Lives in its own test
// package so it links the production library, not the L1 test helpers.
//
//   go test -c -o /data/l2bench.test ./internal/l2bench
//   BENCH_L2_DIR=/data/l2/run1 BENCH_L2_N=1680000 BENCH_L2_PRESIZE=4294967296 \
//     /data/l2bench.test -test.run '^$' -test.bench '^BenchmarkL2Forest$' -test.benchtime 1x
package l2bench

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	utreexo "github.com/utreexo/utreexo"
)

// ---------------------------------------------------------------------------
// Deterministic synthetic chain
// ---------------------------------------------------------------------------

type l2rng struct{ s uint64 }

func (r *l2rng) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *l2rng) below(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func genHashes(seed, n uint64) []utreexo.Hash {
	hs := make([]utreexo.Hash, n)
	x := seed
	var h [32]byte
	for i := uint64(0); i < n; i++ {
		for j := 0; j < 4; j++ {
			x += 0x9E3779B97F4A7C15
			z := x
			z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
			z = (z ^ (z >> 27)) * 0x94D049BB133111EB
			z ^= z >> 31
			binary.LittleEndian.PutUint64(h[j*8:], z)
		}
		hs[i] = utreexo.Hash(h)
	}
	return hs
}

type l2Trace struct {
	hashes   []utreexo.Hash
	addsPer  int
	delPlans [][]uint32 // per block: indices into hashes to delete
}

// genTrace builds a synthetic chain: each block adds `addsPer` new leaves and
// deletes a fraction of currently-live leaves. Deletes always reference a leaf
// added earlier, so positionMap.Get succeeds (the Forest invariant).
func genTrace(seed, n uint64, nBlocks, delPct int) (*l2Trace, error) {
	if nBlocks <= 0 {
		return nil, fmt.Errorf("nBlocks must be > 0")
	}
	addsPer := int(n) / nBlocks
	if addsPer == 0 {
		return nil, fmt.Errorf("nBlocks %d too large for n=%d", nBlocks, n)
	}
	delsPer := addsPer * delPct / 100
	hashes := genHashes(seed, n)
	delPlans := make([][]uint32, nBlocks)
	live := make([]uint32, 0, n)
	r := &l2rng{s: seed ^ 0xABCDEF0123456789}
	next := 0
	for b := 0; b < nBlocks; b++ {
		// Deletes must reference leaves added in *earlier* blocks: modifyInternal
		// applies deletions before additions. Pick from the live set first.
		d := delsPer
		if d > len(live) {
			d = len(live)
		}
		plan := make([]uint32, 0, d)
		for k := 0; k < d; k++ {
			j := r.below(len(live))
			plan = append(plan, live[j])
			live[j] = live[len(live)-1]
			live = live[:len(live)-1]
		}
		delPlans[b] = plan

		for k := 0; k < addsPer && next < int(n); k++ {
			live = append(live, uint32(next))
			next++
		}
	}
	return &l2Trace{hashes: hashes, addsPer: addsPer, delPlans: delPlans}, nil
}

// ---------------------------------------------------------------------------
// /proc + cgroup counters (same shape as the L1 bench)
// ---------------------------------------------------------------------------

type l2Counters struct {
	ReadBytes  uint64
	WriteBytes uint64
	MinFlt     uint64
	MajFlt     uint64
	VmHWMKB    uint64
}

func l2ReadCounters() l2Counters {
	var c l2Counters
	if b, err := os.ReadFile("/proc/self/io"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			f := strings.Fields(ln)
			if len(f) != 2 {
				continue
			}
			v, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "read_bytes:":
				c.ReadBytes = v
			case "write_bytes:":
				c.WriteBytes = v
			}
		}
	}
	if b, err := os.ReadFile("/proc/self/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			f := strings.Fields(s[i+1:])
			if len(f) > 9 {
				c.MinFlt, _ = strconv.ParseUint(f[7], 10, 64)
				c.MajFlt, _ = strconv.ParseUint(f[9], 10, 64)
			}
		}
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(ln, "VmHWM:") {
				f := strings.Fields(ln)
				if len(f) >= 2 {
					c.VmHWMKB, _ = strconv.ParseUint(f[1], 10, 64)
				}
			}
		}
	}
	return c
}

func l2ReadU64(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func l2CgroupDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if i := strings.Index(ln, "::"); i >= 0 {
			return "/sys/fs/cgroup" + strings.TrimSpace(ln[i+2:])
		}
	}
	return ""
}

func l2EnvU64(k string, def uint64) uint64 {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func l2EnvStr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Result
// ---------------------------------------------------------------------------

type l2Result struct {
	Label         string  `json:"label"`
	N             uint64  `json:"n"`
	Blocks        int     `json:"blocks"`
	AddsPerBlock  int     `json:"adds_per_block"`
	DelPct        int     `json:"del_pct"`
	Presize       uint64  `json:"presize"`
	Impl          string  `json:"impl"`
	CacheMB       uint64  `json:"cache_mb"`
	Reopen        bool    `json:"reopen"`
	Leaves        uint64  `json:"leaves"`
	Adds          uint64  `json:"adds"`
	Dels          uint64  `json:"dels"`
	Errors        uint64  `json:"errors"`
	FirstErr      string  `json:"first_err"`
	SetupSeconds  float64 `json:"setup_seconds"`
	TotalSeconds  float64 `json:"total_seconds"`
	BlocksPerSec  float64 `json:"blocks_per_sec"`
	LeavesPerSec  float64 `json:"leaves_per_sec"`
	P50us         int64   `json:"p50_us"`
	P99us         int64   `json:"p99_us"`
	P999us        int64   `json:"p999_us"`
	Maxus         int64   `json:"max_us"`
	Minflt        uint64  `json:"minflt"`
	Majflt        uint64  `json:"majflt"`
	MajfltPerSec  float64 `json:"majflt_per_sec"`
	ReadBytes     uint64  `json:"read_bytes"`
	WriteBytes    uint64  `json:"write_bytes"`
	ReadBytesPerS float64 `json:"read_bytes_per_s"`
	WrBytesPerS   float64 `json:"write_bytes_per_s"`
	VmHWMKB       uint64  `json:"vm_hwm_kb"`
	CgroupPeakMB  float64 `json:"cgroup_peak_mb"`
	ReopenSeconds float64 `json:"reopen_seconds"`
	PosmapBytes   uint64  `json:"posmap_bytes"`
	PosmapSlots   uint64  `json:"posmap_num_slots"`
}

func BenchmarkL2Forest(b *testing.B) {
	n := l2EnvU64("BENCH_L2_N", 1_680_000)
	nBlocks := int(l2EnvU64("BENCH_L2_BLOCKS", 10_000))
	delPct := int(l2EnvU64("BENCH_L2_DEL_PCT", 0))
	presize := l2EnvU64("BENCH_L2_PRESIZE", 2*n)
	impl := l2EnvStr("BENCH_L2_IMPL", "swiss")
	cacheMB := l2EnvU64("BENCH_L2_CACHE_MB", 64)
	seed := l2EnvU64("BENCH_L2_SEED", 0xC0C0_1234)
	dir := l2EnvStr("BENCH_L2_DIR", "")
	label := l2EnvStr("BENCH_L2_LABEL", "run")
	outPath := l2EnvStr("BENCH_L2_OUT", "")
	reopen := l2EnvStr("BENCH_L2_REOPEN", "0") == "1"
	forceRebuild := l2EnvStr("BENCH_L2_FORCE_REBUILD", "0") == "1"

	if dir == "" {
		b.Fatal("BENCH_L2_DIR must be set")
	}
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}

	tr, err := genTrace(seed, n, nBlocks, delPct)
	if err != nil {
		b.Fatal(err)
	}

	opts := []utreexo.ForestOption{
		utreexo.ExpectedMaxLeaves(presize),
		utreexo.MaxCacheMemory(int64(cacheMB) << 20),
		utreexo.PositionMapKind(impl),
	}

	before := l2ReadCounters()
	tSetup := time.Now()
	f, err := utreexo.OpenForest(dir, opts...)
	if err != nil {
		b.Fatal(err)
	}
	setupDur := time.Since(tSetup)

	blockDurs := make([]int64, nBlocks)
	addsBuf := make([]utreexo.Leaf, tr.addsPer)
	delsBuf := make([]utreexo.Hash, 0, 1024)
	var adds, dels, errs uint64
	firstErr := ""

	before = l2ReadCounters()
	t0 := time.Now()
	for bi := 0; bi < nBlocks; bi++ {
		base := bi * tr.addsPer
		for k := 0; k < tr.addsPer; k++ {
			addsBuf[k] = utreexo.Leaf{Hash: tr.hashes[base+k]}
		}
		delsBuf = delsBuf[:0]
		for _, idx := range tr.delPlans[bi] {
			delsBuf = append(delsBuf, tr.hashes[idx])
		}
		t := time.Now()
		if _, _, err := f.ModifyAndReturnTTLs(addsBuf, delsBuf, utreexo.Proof{}); err != nil {
			errs++
			if firstErr == "" {
				firstErr = fmt.Sprintf("block %d: %v", bi, err)
			}
		}
		blockDurs[bi] = time.Since(t).Nanoseconds()
		adds += uint64(len(addsBuf))
		dels += uint64(len(delsBuf))
	}
	total := time.Since(t0)
	after := l2ReadCounters()
	leaves := f.GetNumLeaves()

	p50, p99, p999, maxd := l2Pcts(blockDurs)

	// Clean close with a nonzero commitment so a reopen can reuse the files.
	// A zero commitment (FORCE_REBUILD) leaves the consistency hash invalid, so
	// the reopen below has to rebuild the position map from forest_data.
	closeHash := [32]byte{0x42}
	if forceRebuild {
		closeHash = [32]byte{}
	}
	if err := f.Close(closeHash); err != nil {
		b.Fatal(err)
	}

	// Optional reopen timing (rebuild vs consistency-hash fast path).
	var reopenDur time.Duration
	if reopen {
		rt := time.Now()
		f2, err := utreexo.OpenForest(dir, opts...)
		if err != nil {
			b.Fatal(err)
		}
		reopenDur = time.Since(rt)
		if err := f2.Close([32]byte{0x42}); err != nil {
			b.Fatal(err)
		}
	}

	ctrlBytes, slotsBytes := l2PosmapFileSizes(dir)
	secs := total.Seconds()
	res := l2Result{
		Label:         label,
		N:             n,
		Blocks:        nBlocks,
		AddsPerBlock:  tr.addsPer,
		DelPct:        delPct,
		Presize:       presize,
		Impl:          impl,
		CacheMB:       cacheMB,
		Reopen:        reopen,
		Leaves:        leaves,
		Adds:          adds,
		Dels:          dels,
		Errors:        errs,
		FirstErr:      firstErr,
		SetupSeconds:  setupDur.Seconds(),
		TotalSeconds:  secs,
		BlocksPerSec:  float64(nBlocks) / secs,
		LeavesPerSec:  float64(adds) / secs,
		P50us:         p50,
		P99us:         p99,
		P999us:        p999,
		Maxus:         maxd,
		Minflt:        after.MinFlt - before.MinFlt,
		Majflt:        after.MajFlt - before.MajFlt,
		MajfltPerSec:  float64(after.MajFlt-before.MajFlt) / secs,
		ReadBytes:     after.ReadBytes - before.ReadBytes,
		WriteBytes:    after.WriteBytes - before.WriteBytes,
		ReadBytesPerS: float64(after.ReadBytes-before.ReadBytes) / secs,
		WrBytesPerS:   float64(after.WriteBytes-before.WriteBytes) / secs,
		VmHWMKB:       after.VmHWMKB,
		CgroupPeakMB:  float64(l2ReadU64(filepath.Join(l2CgroupDir(), "memory.peak"))) / (1 << 20),
		ReopenSeconds: reopenDur.Seconds(),
		PosmapBytes:   ctrlBytes + slotsBytes,
		PosmapSlots:   slotsBytes / 8,
	}
	js, _ := json.Marshal(res)
	fmt.Printf("L2RESULT %s\n", js)
	if outPath != "" {
		fh, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fh.Write(js)
			fh.Write([]byte("\n"))
			fh.Close()
		}
	}
}

func l2Pcts(d []int64) (p50, p99, p999, maxd int64) {
	if len(d) == 0 {
		return
	}
	s := append([]int64(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) int64 { return s[int(float64(len(s)-1)*p)] / 1000 }
	return at(0.50), at(0.99), at(0.999), s[len(s)-1] / 1000
}

func l2PosmapFileSizes(dir string) (ctrl, slots uint64) {
	for _, e := range []struct {
		name string
		out  *uint64
	}{
		{"forest_posmap_ctrl.dat", &ctrl},
		{"forest_posmap_slots.dat", &slots},
	} {
		if fi, err := os.Stat(filepath.Join(dir, e.name)); err == nil {
			*e.out = uint64(fi.Size())
		}
	}
	return
}
