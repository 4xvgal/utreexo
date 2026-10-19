//go:build linux

// Extendible-hashing adapter for the L1 position-map harness.
//
// impl "eh"  : 20 B slots (packed value + 12 B key prefix), no verifyHash.
// impl "eh8" : 8 B slots, verifyHash (same access conditions as the bench "lh").
//
// EH_LOAD selects the presize target (default 0.80). A new streaming wall
// benchmark (BenchmarkL1Wall) is provided because the stock harness allocates
// a 32 B key array plus the whole op trace on the heap, which cannot fit under
// the cgroup caps used for P2.
package swisstable

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/utreexo/utreexo/internal/eh"
)

// l1DebugOn defaults to on; BENCH_DEBUG=0 silences progress lines.
func l1DebugOn() bool { return os.Getenv("BENCH_DEBUG") != "0" }

func l1DirBytes(dir string) uint64 {
	if dir == "" {
		return 0
	}
	var n uint64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += uint64(fi.Size())
		}
		return nil
	})
	return n
}

// Watchdog: proves liveness for every impl, including the stock benchmark
// loop we cannot modify. One line per 5 s on stderr.
func init() {
	if !l1DebugOn() {
		return
	}
	go func() {
		t0 := time.Now()
		for {
			time.Sleep(5 * time.Second)
			c := l1ReadCounters()
			fmt.Fprintf(os.Stderr,
				"BENCH_PROGRESS t=%.1fs rss=%dKB minflt=%d majflt=%d read=%dMB write=%dMB cg=%.1fMB mapfiles=%dMB\n",
				time.Since(t0).Seconds(), l1RSSKB(), c.MinFlt, c.MajFlt,
				c.ReadBytes>>20, c.WriteBytes>>20,
				float64(l1ReadU64File(filepath.Join(l1CgroupDir(), "memory.current")))/(1<<20),
				l1DirBytes(os.Getenv("BENCH_DIR"))>>20)
		}
	}()
}

type l1EHMap struct {
	m  *eh.EHMap
	n  uint64
	t0 time.Time
}

func (e *l1EHMap) tick() {
	if !l1DebugOn() {
		return
	}
	e.n++
	if e.n%(1<<20) == 0 {
		fmt.Fprintf(os.Stderr, "EH_DEBUG ops=%d elapsed=%.2fs ops_per_s=%.0f rss=%dKB map=%dMB\n",
			e.n, time.Since(e.t0).Seconds(), float64(e.n)/time.Since(e.t0).Seconds(),
			l1RSSKB(), e.m.MapBytes()>>20)
	}
}

func (e *l1EHMap) Set(h [32]byte, p uint64) error { e.tick(); return e.m.Set(h, p) }
func (e *l1EHMap) Get(h [32]byte) (uint64, bool, error) {
	e.tick()
	return e.m.Get(h)
}
func (e *l1EHMap) Delete(h [32]byte) (bool, error) { e.tick(); return e.m.Delete(h) }
func (e *l1EHMap) Count() uint64                   { return e.m.Count() }
func (e *l1EHMap) ForEach(fn func(uint64))         { e.m.ForEach(fn) }
func (e *l1EHMap) NumSlots() uint64                { return e.m.NumSlots() }
func (e *l1EHMap) MapBytes() uint64                { return e.m.MapBytes() }
func (e *l1EHMap) DropCache()                      { e.m.DropCache() }
func (e *l1EHMap) Close() error                    { return e.m.Close() }

func (e *l1EHMap) Ops() uint64        { return e.m.Ops() }
func (e *l1EHMap) DirBytes() uint64   { return e.m.DirBytes() }
func (e *l1EHMap) Depth() uint64      { return e.m.Depth() }
func (e *l1EHMap) Buckets() uint64    { return e.m.Buckets() }
func (e *l1EHMap) Splits() uint64     { return e.m.Splits() }
func (e *l1EHMap) DirDoubles() uint64 { return e.m.DirDoubles() }
func (e *l1EHMap) Tomb() uint64       { return e.m.Tomb() }
func (e *l1EHMap) PlaceFull() uint64  { return e.m.PlaceFull() }

func l1EHLoad() float64 {
	v := os.Getenv("EH_LOAD")
	if v == "" {
		return 0.80
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0.80
	}
	return f
}

func l1NewEH(dir string, expectedEntries uint64, src HashSource, posMask uint64) (l1Posmap, error) {
	prefix := l1EnvStr("BENCH_IMPL", "eh") != "eh8"
	m, err := eh.NewEHMap(dir, l1EHLoad(), prefix)
	if err != nil {
		return nil, err
	}
	m.SetVerifySource(src, posMask)
	if expectedEntries > 0 {
		if err := m.Presize(expectedEntries); err != nil {
			m.Close()
			return nil, err
		}
	}
	return &l1EHMap{m: m, t0: time.Now()}, nil
}

// ---------------------------------------------------------------------------
// Streaming wall benchmark (P2)
//
// Keys are regenerated on the fly, so the heap holds neither the 32 B key
// array nor the op trace. data.dat is written once (streamed) for the impls
// that use verifyHash, and reused across runs via BENCH_DATA.
// ---------------------------------------------------------------------------

type l1WallResult struct {
	l1Result
	DirBytes   uint64  `json:"dir_bytes"`
	Depth      uint64  `json:"depth"`
	Buckets    uint64  `json:"buckets"`
	Splits     uint64  `json:"splits"`
	DirDoubles uint64  `json:"dir_doubles"`
	Tomb       uint64  `json:"tomb"`
	PlaceFull  uint64  `json:"place_full"`
	Load       float64 `json:"load_observed"`
}

// l1CountMap gives every impl an Ops() counter so the wall sampler can emit
// per-second progress for lh/swiss too (they have no such method).
type l1CountMap struct {
	l1Posmap
	n uint64
}

func (c *l1CountMap) Set(h [32]byte, p uint64) error { c.n++; return c.l1Posmap.Set(h, p) }
func (c *l1CountMap) Get(h [32]byte) (uint64, bool, error) {
	c.n++
	return c.l1Posmap.Get(h)
}
func (c *l1CountMap) Delete(h [32]byte) (bool, error) { c.n++; return c.l1Posmap.Delete(h) }
func (c *l1CountMap) Ops() uint64                     { return c.n }

type l1EHStatter interface {
	Depth() uint64
	Buckets() uint64
	Splits() uint64
	DirDoubles() uint64
	Tomb() uint64
	PlaceFull() uint64
}

func l1WriteData(path string, seed, n uint64) error {
	if fi, err := os.Stat(path); err == nil && uint64(fi.Size()) == n*32 {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const chunk = 2048 // keys per write
	buf := make([]byte, chunk*32)
	for i := uint64(0); i < n; i += chunk {
		m := chunk
		if n-i < uint64(m) {
			m = int(n - i)
		}
		for j := 0; j < m; j++ {
			h := l1Key(seed, i+uint64(j))
			copy(buf[j*32:], h[:])
		}
		if _, err := f.Write(buf[:m*32]); err != nil {
			return err
		}
	}
	return f.Sync()
}

type l1Sample struct {
	Ts         float64 `json:"t"`
	ElapsedS   float64 `json:"elapsed_s"`
	Ops        uint64  `json:"ops"`
	OpsPerS    float64 `json:"ops_per_s"`
	RssKB      uint64  `json:"rss_kb"`
	MapBytes   uint64  `json:"map_bytes"`
	MinFlt     uint64  `json:"minflt"`
	MajFlt     uint64  `json:"majflt"`
	ReadBytes  uint64  `json:"read_bytes"`
	WriteBytes uint64  `json:"write_bytes"`
}

func l1RSSKB() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, ln := range splitLines(string(b)) {
		if len(ln) > 6 && ln[:6] == "VmRSS:" {
			f := fields(ln)
			if len(f) >= 2 {
				v, _ := strconv.ParseUint(f[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

func fields(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

func BenchmarkL1Wall(b *testing.B) {
	n := l1EnvU64("BENCH_N", 16<<20)
	impl := l1EnvStr("BENCH_IMPL", "eh")
	seed := l1EnvU64("BENCH_SEED", 0xC0C0_1234)
	dir := l1EnvStr("BENCH_DIR", "")
	label := l1EnvStr("BENCH_LABEL", "wall")
	outPath := l1EnvStr("BENCH_OUT", "")
	cold := l1EnvStr("BENCH_COLD", "1") == "1"
	if dir == "" {
		b.Fatal("BENCH_DIR must be set")
	}
	if l1EnvStr("BENCH_MIX", "add") != "add" {
		b.Fatal("BenchmarkL1Wall only supports BENCH_MIX=add")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}

	needData := impl == "swiss" || impl == "lh" || impl == "eh8"
	var src HashSource
	var dataMM []byte
	var dataF *os.File
	if needData {
		dataPath := l1EnvStr("BENCH_DATA", filepath.Join(dir, "data.dat"))
		if err := l1WriteData(dataPath, seed, n); err != nil {
			b.Fatal(err)
		}
		var err error
		dataF, err = os.Open(dataPath)
		if err != nil {
			b.Fatal(err)
		}
		dataMM, err = syscall.Mmap(int(dataF.Fd()), 0, int(n*32), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			b.Fatal(err)
		}
		src = l1SliceSource{data: dataMM}
		defer func() {
			if dataMM != nil {
				syscall.Munmap(dataMM)
			}
			dataF.Close()
		}()
	}

	tSetup := time.Now()
	sizeHint := n
	if impl == "swiss" {
		sizeHint = l1EnvU64("BENCH_PRESIZE", 2*n)
	}
	m, err := l1NewImpl(impl, dir, sizeHint, src, testPositionMask)
	if err != nil {
		b.Fatal(err)
	}
	setupDur := time.Since(tSetup)
	defer m.Close()
	raw := m
	m = &l1CountMap{l1Posmap: m}
	if cold {
		m.DropCache()
	}

	// Sampler: one line per second, written next to the run's results.
	samplesPath := os.Getenv("EH_SAMPLES")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	started := time.Now()
	if samplesPath != "" {
		f, err := os.Create(samplesPath)
		if err != nil {
			b.Fatal(err)
		}
		if oc, ok := m.(interface{ Ops() uint64 }); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer f.Close()
				enc := json.NewEncoder(f)
				lastT := started
				lastOps := uint64(0)
				emit := func() {
					c := l1ReadCounters()
					now := time.Now()
					ops := oc.Ops()
					s := l1Sample{
						Ts:         now.Sub(started).Seconds(),
						ElapsedS:   now.Sub(started).Seconds(),
						Ops:        ops,
						OpsPerS:    float64(ops-lastOps) / now.Sub(lastT).Seconds(),
						RssKB:      l1RSSKB(),
						MapBytes:   m.MapBytes(),
						MinFlt:     c.MinFlt,
						MajFlt:     c.MajFlt,
						ReadBytes:  c.ReadBytes,
						WriteBytes: c.WriteBytes,
					}
					lastT, lastOps = now, ops
					enc.Encode(s)
				}
				tk := time.NewTicker(time.Second)
				defer tk.Stop()
				for {
					select {
					case <-stop:
						emit()
						return
					case <-tk.C:
						emit()
					}
				}
			}()
		} else {
			f.Close()
		}
	}

	before := l1ReadCounters()
	t0 := time.Now()
	for i := uint64(0); i < n; i++ {
		if l1DebugOn() && i > 0 && i%(1<<20) == 0 {
			c := l1ReadCounters()
			el := time.Since(t0).Seconds()
			fmt.Fprintf(os.Stderr,
				"WALL_PROGRESS ops=%d elapsed=%.1fs ops_per_s=%.0f rss=%dKB map=%dMB majflt=%d read=%dMB write=%dMB\n",
				i, el, float64(i)/el, l1RSSKB(), m.MapBytes()>>20,
				c.MajFlt-before.MajFlt, (c.ReadBytes-before.ReadBytes)>>20, (c.WriteBytes-before.WriteBytes)>>20)
		}
		h := l1Key(seed, i)
		if err := m.Set(h, i); err != nil {
			b.Fatalf("Set %d: %v", i, err)
		}
	}
	total := time.Since(t0)
	close(stop)
	wg.Wait()
	after := l1ReadCounters()

	secs := total.Seconds()
	res := l1WallResult{l1Result: l1Result{
		Label:          label,
		Impl:           impl,
		N:              n,
		Presize:        sizeHint,
		Mix:            "add",
		Mode:           "stream",
		Seed:           seed,
		Cold:           cold,
		Ops:            int(n),
		Adds:           int(n),
		Errors:         0,
		GetsFound:      0,
		DelsDone:       0,
		NumSlots:       m.NumSlots(),
		MapTotalBytes:  m.MapBytes(),
		SetupSeconds:   setupDur.Seconds(),
		TotalSeconds:   secs,
		OpsPerSec:      float64(n) / secs,
		Minflt:         after.MinFlt - before.MinFlt,
		Majflt:         after.MajFlt - before.MajFlt,
		MajfltPerSec:   float64(after.MajFlt-before.MajFlt) / secs,
		MinfltPerSec:   float64(after.MinFlt-before.MinFlt) / secs,
		ReadBytes:      after.ReadBytes - before.ReadBytes,
		WriteBytes:     after.WriteBytes - before.WriteBytes,
		ReadBytesPerS:  float64(after.ReadBytes-before.ReadBytes) / secs,
		WriteBytesPerS: float64(after.WriteBytes-before.WriteBytes) / secs,
		VmHWMKB:        after.VmHWMKB,
		CgroupPeakMB:   float64(l1ReadU64File(filepath.Join(l1CgroupDir(), "memory.peak"))) / (1 << 20),
		CgroupCurMB:    float64(l1ReadU64File(filepath.Join(l1CgroupDir(), "memory.current"))) / (1 << 20),
	}}
	if st, ok := raw.(l1EHStatter); ok {
		res.DirBytes = uint64(0)
		if d, ok := raw.(interface{ DirBytes() uint64 }); ok {
			res.DirBytes = d.DirBytes()
		}
		res.Depth = st.Depth()
		res.Buckets = st.Buckets()
		res.Splits = st.Splits()
		res.DirDoubles = st.DirDoubles()
		res.Tomb = st.Tomb()
		res.PlaceFull = st.PlaceFull()
		if slots := raw.NumSlots(); slots > 0 {
			res.Load = float64(raw.Count()) / float64(slots)
		}
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
