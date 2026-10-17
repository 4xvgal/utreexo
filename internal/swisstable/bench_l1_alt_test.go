//go:build linux

// L1 alternative position-map implementations, benchmark-only.
//
// Everything here lives in *_test.go so it is linked into the benchmark binary
// only; the production library (swisstable.go, forest.go) is untouched.
//
// Implementations:
//   swiss  - production SwissPositionMap (presize controlled by BENCH_PRESIZE)
//   lh     - hand-written linear hashing on an mmap'd file: one bucket split
//            at a time, no O(N) rehash, no presize.
//   bbolt  - go.etcd.io/bbolt (mmap B+tree); stores the full 32B key, so it
//            does not need verifyHash at all.
package swisstable

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	bolt "go.etcd.io/bbolt"
)

// l1Posmap is the benchmark-facing interface. It matches the subset of
// SwissPositionMap used by Forest (Get/Set/Delete/Count/ForEach/Close).
type l1Posmap interface {
	Set(hash [32]byte, packed uint64) error
	Get(hash [32]byte) (uint64, bool, error)
	Delete(hash [32]byte) (bool, error)
	Count() uint64
	ForEach(fn func(packed uint64))
	NumSlots() uint64
	MapBytes() uint64
	DropCache()
	Close() error
}

func l1NewImpl(impl, dir string, expectedEntries uint64, src HashSource, posMask uint64) (l1Posmap, error) {
	switch impl {
	case "swiss", "":
		ctrlPath := filepath.Join(dir, "ctrl.dat")
		slotsPath := filepath.Join(dir, "slots.dat")
		os.Remove(ctrlPath)
		os.Remove(slotsPath)
		m, _, err := NewSwissPositionMap(ctrlPath, slotsPath, expectedEntries, [32]byte{}, src, posMask)
		if err != nil {
			return nil, err
		}
		return &l1SwissPosmap{m: m}, nil
	case "lh":
		return l1NewLH(dir, expectedEntries, src, posMask)
	case "bbolt":
		return l1NewBbolt(dir)
	default:
		return nil, fmt.Errorf("bench_l1: unknown BENCH_IMPL %q", impl)
	}
}

// ---------------------------------------------------------------------------
// swiss adapter
// ---------------------------------------------------------------------------

type l1SwissPosmap struct{ m *SwissPositionMap }

func (s *l1SwissPosmap) Set(h [32]byte, p uint64) error         { return s.m.Set(h, p) }
func (s *l1SwissPosmap) Get(h [32]byte) (uint64, bool, error)   { return s.m.Get(h) }
func (s *l1SwissPosmap) Delete(h [32]byte) (bool, error)        { return s.m.Delete(h) }
func (s *l1SwissPosmap) Count() uint64                          { return s.m.Count() }
func (s *l1SwissPosmap) ForEach(fn func(uint64))                { s.m.ForEach(fn) }
func (s *l1SwissPosmap) NumSlots() uint64                       { return s.m.numSlots }
func (s *l1SwissPosmap) DropCache()                             { l1DropCache(s.m.ctrlFile); l1DropCache(s.m.slotsFile) }
func (s *l1SwissPosmap) Close() error                           { return s.m.Close() }
func (s *l1SwissPosmap) MapBytes() uint64 {
	return uint64(fileHeaderSize) + s.m.numSlots + uint64(fileHeaderSize) + s.m.numSlots*8
}

// ---------------------------------------------------------------------------
// lh: linear hashing
//
// File = 64B header + buckets. Bucket = ctrl[128] (1B/slot) + slots[128]*8.
// Growth: one bucket is split at a time when global load > 0.5, so the pause
// is bounded by one bucket (~128 verifyHash reads), independent of N.
// Bucket capacity 128 at load 0.5 means per-bucket overflow is statistically
// impossible (Poisson mean 64, max ~110 << 128); a full bucket forces a split.
// ---------------------------------------------------------------------------

const (
	lhBucketSlots = 128
	lhHeaderSize  = 64
	lhBucketSize  = lhBucketSlots + lhBucketSlots*8
)

type l1LHMap struct {
	mmap    []byte
	file    *os.File
	level   uint64
	splitAt uint64
	count   uint64
	buckets uint64 // allocated capacity
	src     HashSource
	posMask uint64
}

func l1NewLH(dir string, expectedEntries uint64, src HashSource, posMask uint64) (*l1LHMap, error) {
	// Size for the final count at load 0.5, with 2x margin for power-of-two growth.
	need := uint64(1)
	if expectedEntries > 0 {
		need = expectedEntries*2/lhBucketSlots + 1
	}
	maxB := uint64(2)
	for maxB < need*2 {
		maxB <<= 1
	}
	size := int64(lhHeaderSize + maxB*lhBucketSize)
	path := filepath.Join(dir, "lh.dat")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, err
	}
	mm, err := syscall.Mmap(int(f.Fd()), 0, int(size),
		syscall.PROT_READ|syscall.PROT_WRITE, mmapFlags)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &l1LHMap{mmap: mm, file: f, buckets: maxB, src: src, posMask: posMask}, nil
}

func (m *l1LHMap) numBuckets() uint64 { return (uint64(1) << m.level) + m.splitAt }

func (m *l1LHMap) bucketFor(h1 uint64) uint64 {
	base := uint64(1) << m.level
	b := h1 % (base << 1)
	if b >= base+m.splitAt {
		b = h1 % base
	}
	return b
}

func (m *l1LHMap) ctrl(b uint64) []byte {
	o := lhHeaderSize + int(b)*lhBucketSize
	return m.mmap[o : o+lhBucketSlots]
}

func (m *l1LHMap) slots(b uint64) []byte {
	o := lhHeaderSize + int(b)*lhBucketSize + lhBucketSlots
	return m.mmap[o : o+lhBucketSlots*8]
}

func (m *l1LHMap) getSlot(b, i uint64) uint64 {
	return binary.LittleEndian.Uint64(m.slots(b)[i*8:])
}

func (m *l1LHMap) setSlot(b, i, v uint64) {
	binary.LittleEndian.PutUint64(m.slots(b)[i*8:], v)
}

func (m *l1LHMap) clearBucket(b uint64) {
	c := m.ctrl(b)
	for i := range c {
		c[i] = 0
	}
	s := m.slots(b)
	for i := range s {
		s[i] = 0
	}
}

func (m *l1LHMap) verify(packed uint64, expected [32]byte) (bool, error) {
	pos := packed & m.posMask
	h, err := m.src.HashAt(int64(pos * 32))
	if err != nil {
		return false, err
	}
	return h == expected, nil
}

func (m *l1LHMap) Set(hash [32]byte, packed uint64) error {
	h1, h2 := splitHash(hash)
	for {
		b := m.bucketFor(h1)
		ctrl := m.ctrl(b)
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == h2 {
				ok, err := m.verify(m.getSlot(b, uint64(i)), hash)
				if err != nil {
					return err
				}
				if ok {
					m.setSlot(b, uint64(i), packed)
					return nil
				}
			}
		}
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == ctrlEmpty || ctrl[i] == ctrlDeleted {
				ctrl[i] = h2
				m.setSlot(b, uint64(i), packed)
				m.count++
				for m.count*2 > m.numBuckets()*lhBucketSlots { // load > 0.5
					if err := m.split(); err != nil {
						return err
					}
				}
				return nil
			}
		}
		// bucket unexpectedly full: split and retry
		if err := m.split(); err != nil {
			return err
		}
	}
}

// place inserts a known-unique entry (used by split reinsertion).
func (m *l1LHMap) place(b uint64, hash [32]byte, packed uint64) error {
	_, h2 := splitHash(hash)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == ctrlEmpty || ctrl[i] == ctrlDeleted {
			ctrl[i] = h2
			m.setSlot(b, uint64(i), packed)
			return nil
		}
	}
	return fmt.Errorf("lh place: bucket %d full", b)
}

func (m *l1LHMap) split() error {
	oldB := m.splitAt
	newB := (uint64(1) << m.level) + m.splitAt
	if newB >= m.buckets {
		return fmt.Errorf("lh: out of buckets (new=%d max=%d)", newB, m.buckets)
	}
	m.clearBucket(newB)
	m.splitAt++
	if m.splitAt == (uint64(1) << m.level) {
		m.level++
		m.splitAt = 0
	}
	// copy old bucket entries, then redistribute
	type ent struct {
		packed uint64
	}
	ents := make([]ent, 0, lhBucketSlots)
	ctrl := m.ctrl(oldB)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i]&ctrlFull != 0 {
			ents = append(ents, ent{packed: m.getSlot(oldB, uint64(i))})
		}
	}
	m.clearBucket(oldB)
	for _, e := range ents {
		h, err := m.src.HashAt(int64((e.packed & m.posMask) * 32))
		if err != nil {
			return err
		}
		h1 := binary.LittleEndian.Uint64(h[:8])
		if err := m.place(m.bucketFor(h1), h, e.packed); err != nil {
			return err
		}
	}
	return nil
}

func (m *l1LHMap) Get(hash [32]byte) (uint64, bool, error) {
	h1, h2 := splitHash(hash)
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == h2 {
			packed := m.getSlot(b, uint64(i))
			ok, err := m.verify(packed, hash)
			if err != nil {
				return 0, false, err
			}
			if ok {
				return packed, true, nil
			}
		}
	}
	return 0, false, nil
}

func (m *l1LHMap) Delete(hash [32]byte) (bool, error) {
	h1, h2 := splitHash(hash)
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == h2 {
			ok, err := m.verify(m.getSlot(b, uint64(i)), hash)
			if err != nil {
				return false, err
			}
			if ok {
				ctrl[i] = ctrlDeleted
				m.setSlot(b, uint64(i), 0)
				m.count--
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *l1LHMap) Count() uint64 { return m.count }

func (m *l1LHMap) ForEach(fn func(uint64)) {
	for b := uint64(0); b < m.numBuckets(); b++ {
		ctrl := m.ctrl(b)
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i]&ctrlFull != 0 {
				fn(m.getSlot(b, uint64(i)))
			}
		}
	}
}

func (m *l1LHMap) NumSlots() uint64 { return m.numBuckets() * lhBucketSlots }
func (m *l1LHMap) MapBytes() uint64 { return uint64(len(m.mmap)) }
func (m *l1LHMap) DropCache()       { l1DropCache(m.file) }

func (m *l1LHMap) Close() error {
	err := syscall.Munmap(m.mmap)
	if e := m.file.Close(); err == nil {
		err = e
	}
	return err
}

// ---------------------------------------------------------------------------
// bbolt: mmap B+tree storing the full key
// ---------------------------------------------------------------------------

type l1BboltMap struct {
	db    *bolt.DB
	tx    *bolt.Tx
	b     *bolt.Bucket
	count uint64
	mut   uint64
	path  string
}

const l1BboltCommitEvery = 100_000

func l1NewBbolt(dir string) (*l1BboltMap, error) {
	path := filepath.Join(dir, "bolt.db")
	os.Remove(path)
	db, err := bolt.Open(path, 0o644, &bolt.Options{
		NoSync:       true,
		NoGrowSync:   true,
		FreelistType: bolt.FreelistMapType,
	})
	if err != nil {
		return nil, err
	}
	b := &l1BboltMap{db: db, path: path}
	if err := b.begin(); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

func (b *l1BboltMap) begin() error {
	tx, err := b.db.Begin(true)
	if err != nil {
		return err
	}
	bkt, err := tx.CreateBucketIfNotExists([]byte("p"))
	if err != nil {
		tx.Rollback()
		return err
	}
	b.tx, b.b = tx, bkt
	return nil
}

func (b *l1BboltMap) afterMut() error {
	b.mut++
	if b.mut%l1BboltCommitEvery != 0 {
		return nil
	}
	if err := b.tx.Commit(); err != nil {
		return err
	}
	return b.begin()
}

func (b *l1BboltMap) Set(hash [32]byte, packed uint64) error {
	var v [8]byte
	binary.LittleEndian.PutUint64(v[:], packed)
	if err := b.b.Put(hash[:], v[:]); err != nil {
		return err
	}
	b.count++
	return b.afterMut()
}

func (b *l1BboltMap) Get(hash [32]byte) (uint64, bool, error) {
	v := b.b.Get(hash[:])
	if v == nil {
		return 0, false, nil
	}
	if len(v) != 8 {
		return 0, false, fmt.Errorf("bbolt: value len %d", len(v))
	}
	return binary.LittleEndian.Uint64(v), true, nil
}

func (b *l1BboltMap) Delete(hash [32]byte) (bool, error) {
	if b.b.Get(hash[:]) == nil {
		return false, nil
	}
	if err := b.b.Delete(hash[:]); err != nil {
		return false, err
	}
	b.count--
	return true, b.afterMut()
}

func (b *l1BboltMap) Count() uint64 { return b.count }

func (b *l1BboltMap) ForEach(fn func(uint64)) {
	b.b.ForEach(func(_, v []byte) error {
		if len(v) == 8 {
			fn(binary.LittleEndian.Uint64(v))
		}
		return nil
	})
}

func (b *l1BboltMap) NumSlots() uint64 { return 0 }
func (b *l1BboltMap) MapBytes() uint64 {
	fi, err := os.Stat(b.path)
	if err != nil {
		return 0
	}
	return uint64(fi.Size())
}

func (b *l1BboltMap) DropCache() {
	if f, err := os.Open(b.path); err == nil {
		l1DropCache(f)
		f.Close()
	}
}

func (b *l1BboltMap) Close() error {
	if b.tx != nil {
		b.tx.Commit()
		b.tx = nil
	}
	return b.db.Close()
}
