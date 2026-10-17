//go:build linux

// L1 independent benchmark for the utreexo position map.
//
// This file lives inside the swisstable package because internal/swisstable
// cannot be imported from outside the module. It compares the production
// SwissPositionMap at different presize (expectedEntries) settings under the
// same deterministic op trace.
//
// See docs/utreexo-posmap-benchmark-plan.md in coconut_wallet.
//
// Run (compiled once, run many):
//
//	go test -c -o /data/bench_l1.test ./internal/swisstable
//	BENCH_DIR=/data/run1 BENCH_N=2000000 BENCH_PRESIZE=4294967296 \
//	BENCH_MIX=add BENCH_MODE=A BENCH_OUT=/data/l1-results.jsonl \
//	  /data/bench_l1.test -test.run '^$' -test.bench '^BenchmarkL1Posmap$' -test.benchtime 1x
package swisstable

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Deterministic key generation (splitmix64). Same seed => same keys, so every
// implementation and every presize setting replays the identical trace.
// ---------------------------------------------------------------------------

type l1rng struct{ s uint64 }

func (r *l1rng) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *l1rng) below(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	return r.next() % n
}

// l1Key derives the i-th 32-byte key for a seed. The value is written to the
// data file at i*32, which is what SwissPositionMap.verifyHash reads back.
func l1Key(seed, i uint64) [32]byte {
	var h [32]byte
	x := seed ^ (i + 0x9E3779B97F4A7C15)
	for j := 0; j < 4; j++ {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		binary.LittleEndian.PutUint64(h[j*8:], z)
	}
	return h
}

// ---------------------------------------------------------------------------
// Op trace
// ---------------------------------------------------------------------------

const l1DelBit = uint64(1) << 63

// l1GenOps builds a deterministic op trace. kind=add stores the key index;
// kind=delete stores l1DelBit|keyIndex. Deletes always target an active key,
// so positionMap.Get succeeds (the forest invariant from the plan).
func l1GenOps(seed, n uint64, mix string) []uint64 {
	var nDel uint64
	switch mix {
	case "add", "add-only", "1:0":
		nDel = 0
	case "2:1":
		nDel = n / 2
	case "1:1":
		nDel = n
	case "1:2":
		nDel = 2 * n
	default:
		panic("bench_l1: unknown BENCH_MIX " + mix)
	}

	ops := make([]uint64, 0, n+nDel)
	active := make([]uint64, 0, n)
	r := &l1rng{s: seed ^ 0xD1B54A32D192ED03}

	addsLeft, delsLeft := n, nDel
	for addsLeft > 0 || delsLeft > 0 {
		doAdd := addsLeft > 0
		if doAdd && delsLeft > 0 && len(active) > 0 {
			// Keep the running add:del ratio probabilistic.
			doAdd = r.below(addsLeft+delsLeft) < addsLeft
		}
		if doAdd {
			idx := n - addsLeft
			active = append(active, idx)
			ops = append(ops, idx)
			addsLeft--
			continue
		}
		j := r.below(uint64(len(active)))
		idx := active[j]
		active[j] = active[len(active)-1]
		active = active[:len(active)-1]
		ops = append(ops, l1DelBit|idx)
		delsLeft--
	}
	return ops
}

// ---------------------------------------------------------------------------
// HashSource modes
// ---------------------------------------------------------------------------

// l1SliceSource is an in-memory HashSource. Mode A backs it with an mmap of
// data.dat; mode B backs it with a plain Go heap slice (no file I/O for
// verifyHash).
type l1SliceSource struct{ data []byte }

func (s l1SliceSource) HashAt(off int64) ([32]byte, error) {
	return *(*[32]byte)(s.data[off : off+32]), nil
}

// ---------------------------------------------------------------------------
// /proc and cgroup counters
// ---------------------------------------------------------------------------

type l1Counters struct {
	RChar      uint64 `json:"rchar"`
	WChar      uint64 `json:"wchar"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
	MinFlt     uint64 `json:"minflt"`
	MajFlt     uint64 `json:"majflt"`
	VmHWMKB    uint64 `json:"vm_hwm_kb"`
}

func l1ReadCounters() l1Counters {
	var c l1Counters
	if b, err := os.ReadFile("/proc/self/io"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			f := strings.Fields(ln)
			if len(f) != 2 {
				continue
			}
			v, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "rchar:":
				c.RChar = v
			case "wchar:":
				c.WChar = v
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
			// After ')': state(0) ppid(1) pgrp(2) session(3) tty(4)
			// tpgid(5) flags(6) minflt(7) cminflt(8) majflt(9)
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

func l1ReadU64File(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}

// l1CgroupDir resolves the process's own cgroup v2 directory. Inside the VM
// /sys/fs/cgroup is the root, so memory.peak must be read from the scope the
// process actually runs in (e.g. systemd-run's .scope).
func l1CgroupDir() string {
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

// l1DropCache asks the kernel to drop clean page cache for a file
// (posix_fadvise FADV_DONTNEED). Used for the cold-cache scenario.
func l1DropCache(f *os.File) {
	if f == nil {
		return
	}
	const fadvDontNeed = 4
	syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, fadvDontNeed, 0, 0)
}

// ---------------------------------------------------------------------------
// Result record
// ---------------------------------------------------------------------------

type l1Result struct {
	Label          string  `json:"label"`
	Impl           string  `json:"impl"`
	Commit         string  `json:"commit"`
	N              uint64  `json:"n"`
	Presize        uint64  `json:"presize"`
	Mix            string  `json:"mix"`
	Mode           string  `json:"mode"`
	Seed           uint64  `json:"seed"`
	Cold           bool    `json:"cold"`
	Ops            int     `json:"ops"`
	Adds           int     `json:"adds"`
	Dels           int     `json:"dels"`
	Errors         uint64  `json:"errors"`
	GetsFound      uint64  `json:"gets_found"`
	DelsDone       uint64  `json:"dels_done"`
	NumSlots       uint64  `json:"num_slots"`
	CtrlBytes      uint64  `json:"ctrl_bytes"`
	SlotsBytes     uint64  `json:"slots_bytes"`
	MapTotalBytes  uint64  `json:"map_total_bytes"`
	SetupSeconds   float64 `json:"setup_seconds"`
	MmapSeconds    float64 `json:"mmap_seconds"`
	TotalSeconds   float64 `json:"total_seconds"`
	OpsPerSec      float64 `json:"ops_per_sec"`
	P50us          int64   `json:"p50_us"`
	P99us          int64   `json:"p99_us"`
	P999us         int64   `json:"p999_us"`
	Maxus          int64   `json:"max_us"`
	Meanus         float64 `json:"mean_us"`
	Minflt         uint64  `json:"minflt"`
	Majflt         uint64  `json:"majflt"`
	MajfltPerSec   float64 `json:"majflt_per_sec"`
	MinfltPerSec   float64 `json:"minflt_per_sec"`
	ReadBytes      uint64  `json:"read_bytes"`
	WriteBytes     uint64  `json:"write_bytes"`
	ReadBytesPerS  float64 `json:"read_bytes_per_s"`
	WriteBytesPerS float64 `json:"write_bytes_per_s"`
	VmHWMKB        uint64  `json:"vm_hwm_kb"`
	CgroupPeakMB   float64 `json:"cgroup_peak_mb"`
	CgroupCurMB    float64 `json:"cgroup_current_mb"`
}

// ---------------------------------------------------------------------------
// Benchmark
// ---------------------------------------------------------------------------

func l1EnvStr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func l1EnvU64(k string, def uint64) uint64 {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func l1KeyAt(keys []byte, i uint64) [32]byte {
	return *(*[32]byte)(keys[i*32 : i*32+32])
}

func BenchmarkL1Posmap(b *testing.B) {
	n := l1EnvU64("BENCH_N", 1_000_000)
	presize := l1EnvU64("BENCH_PRESIZE", 2*n)
	mode := l1EnvStr("BENCH_MODE", "A")
	impl := l1EnvStr("BENCH_IMPL", "swiss")
	mix := l1EnvStr("BENCH_MIX", "add")
	seed := l1EnvU64("BENCH_SEED", 0xC0C0_1234)
	dir := l1EnvStr("BENCH_DIR", "")
	label := l1EnvStr("BENCH_LABEL", "run")
	outPath := l1EnvStr("BENCH_OUT", "")
	cold := l1EnvStr("BENCH_COLD", "1") == "1"

	if dir == "" {
		b.Fatal("BENCH_DIR must be set (must live on the named volume, not tmpfs)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	dataPath := filepath.Join(dir, "data.dat")

	// --- keys / data file -------------------------------------------------
	keys := make([]byte, n*32)
	for i := uint64(0); i < n; i++ {
		h := l1Key(seed, i)
		copy(keys[i*32:], h[:])
	}
	if fi, err := os.Stat(dataPath); err != nil || uint64(fi.Size()) != n*32 {
		df, err := os.Create(dataPath)
		if err != nil {
			b.Fatal(err)
		}
		bw := bufio.NewWriterSize(df, 1<<20)
		if _, err := bw.Write(keys); err != nil {
			b.Fatal(err)
		}
		if err := bw.Flush(); err != nil {
			b.Fatal(err)
		}
		if err := df.Sync(); err != nil {
			b.Fatal(err)
		}
		df.Close()
	}

	var src HashSource
	var dataMmap []byte
	var dataFile *os.File
	if mode == "B" {
		raw, err := os.ReadFile(dataPath)
		if err != nil {
			b.Fatal(err)
		}
		// Copy onto the Go heap so verifyHash never touches the page cache.
		heap := make([]byte, len(raw))
		copy(heap, raw)
		src = l1SliceSource{data: heap}
	} else {
		var err error
		dataFile, err = os.Open(dataPath)
		if err != nil {
			b.Fatal(err)
		}
		dataMmap, err = syscall.Mmap(int(dataFile.Fd()), 0, int(n*32),
			syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			b.Fatal(err)
		}
		src = l1SliceSource{data: dataMmap}
	}
	defer func() {
		if dataMmap != nil {
			syscall.Munmap(dataMmap)
		}
		if dataFile != nil {
			dataFile.Close()
		}
	}()

	ops := l1GenOps(seed, n, mix)

	// --- construct map ----------------------------------------------------
	tSetup := time.Now()
	sizeHint := presize
	if impl != "swiss" {
		sizeHint = n // alt structures are sized by live count, not presize
	}
	m, err := l1NewImpl(impl, dir, sizeHint, src, testPositionMask)
	if err != nil {
		b.Fatal(err)
	}
	setupDur := time.Since(tSetup)
	defer m.Close()

	if cold {
		// Undo MAP_POPULATE warmup so the measured loop sees cold pages.
		m.DropCache()
	}

	// --- timed replay -----------------------------------------------------
	durations := make([]int64, len(ops))
	var adds, dels, errs, getsFound, delsDone uint64

	before := l1ReadCounters()
	t0 := time.Now()
	for i, o := range ops {
		t := time.Now()
		if o&l1DelBit != 0 {
			idx := o &^ l1DelBit
			dels++
			h := l1KeyAt(keys, idx)
			_, found, err := m.Get(h)
			if err != nil {
				errs++
			} else if found {
				getsFound++
				if _, err := m.Delete(h); err != nil {
					errs++
				} else {
					delsDone++
				}
			}
		} else {
			adds++
			h := l1KeyAt(keys, o)
			if err := m.Set(h, o); err != nil {
				errs++
			}
		}
		durations[i] = time.Since(t).Nanoseconds()
	}
	total := time.Since(t0)
	after := l1ReadCounters()

	// --- stats ------------------------------------------------------------
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	pct := func(p float64) int64 {
		if len(durations) == 0 {
			return 0
		}
		i := int(float64(len(durations)-1) * p)
		return durations[i] / 1000 // ns -> us
	}
	var sum int64
	for _, d := range durations {
		sum += d
	}
	meanus := 0.0
	if len(durations) > 0 {
		meanus = float64(sum) / float64(len(durations)) / 1000
	}

	numSlots := m.NumSlots()
	mapBytes := m.MapBytes()
	var ctrlBytes, slotsBytes uint64
	if impl == "swiss" {
		ctrlBytes = uint64(fileHeaderSize) + numSlots
		slotsBytes = uint64(fileHeaderSize) + numSlots*8
	}

	majFlt := after.MajFlt - before.MajFlt
	minFlt := after.MinFlt - before.MinFlt
	readBytes := after.ReadBytes - before.ReadBytes
	writeBytes := after.WriteBytes - before.WriteBytes
	secs := total.Seconds()

	res := l1Result{
		Label:          label,
		N:              n,
		Presize:        presize,
		Mix:            mix,
		Mode:           mode,
		Seed:           seed,
		Cold:           cold,
		Ops:            len(ops),
		Adds:           int(adds),
		Dels:           int(dels),
		Errors:         errs,
		GetsFound:      getsFound,
		DelsDone:       delsDone,
		Impl:           impl,
		NumSlots:       numSlots,
		CtrlBytes:      ctrlBytes,
		SlotsBytes:     slotsBytes,
		MapTotalBytes:  mapBytes,
		SetupSeconds:   setupDur.Seconds(),
		TotalSeconds:   secs,
		OpsPerSec:      float64(len(ops)) / secs,
		P50us:          pct(0.50),
		P99us:          pct(0.99),
		P999us:         pct(0.999),
		Maxus:          pct(1.0),
		Meanus:         meanus,
		Minflt:         minFlt,
		Majflt:         majFlt,
		MajfltPerSec:   float64(majFlt) / secs,
		MinfltPerSec:   float64(minFlt) / secs,
		ReadBytes:      readBytes,
		WriteBytes:     writeBytes,
		ReadBytesPerS:  float64(readBytes) / secs,
		WriteBytesPerS: float64(writeBytes) / secs,
		VmHWMKB:        after.VmHWMKB,
		CgroupPeakMB:   float64(l1ReadU64File(filepath.Join(l1CgroupDir(), "memory.peak"))) / (1 << 20),
		CgroupCurMB:    float64(l1ReadU64File(filepath.Join(l1CgroupDir(), "memory.current"))) / (1 << 20),
	}

	js, _ := json.Marshal(res)
	fmt.Printf("L1RESULT %s\n", js)
	if outPath != "" {
		f, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(js)
			f.Write([]byte("\n"))
			f.Close()
		}
	}
}
