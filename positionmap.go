// positionMap is the abstraction Forest uses to a map leaf hashes
// to packed (addIndex, position) values.
// Current implementaion is the Swiss Table in
// internal/swisstable, exposed through swissPM.

package utreexo

import (
	"github.com/utreexo/utreexo/internal/swisstable"
)

type PositionMap interface {
	// Get returns the packed value for hash.
	Get(hash [32]byte) (uint64, bool, error)
	// Set inserts or overwrites the entry for hash.
	Set(hash [32]byte, packed uint64) error
	// Delete removes hash and reports whether it was present.
	Delete(hash [32]byte) (bool, error)
	// Count returns the number of entries in the map.
	Count() uint64
	// ForEach calls fn for every entry. The order is unspcified and
	// fn must not mutate the map
	ForEach(fn func(packed uint64))
	// Close releases the backing files.
	Close() error
	//
	SetConsistencyHash(hash [32]byte) error
	// BeginBatch pre-sizes the map for up to maxEntries inserts.
	BeginBatch(maxEntries uint64) (PositionMapBatch, error)
}

// PositionMapBatch is the bulk-insert handle returned by [PositionMap.BeginBatch].
type PositionMapBatch interface {
	Insert(hash [32]byte, packed uint64) error
}

// swissPM adapts swisstable.SwissPositionMap to PositionMap.
// wrapping only that call keeps internal/swisstable unchanged.
type swissPM struct{ m *swisstable.SwissPositionMap }

func (s *swissPM) Get(h [32]byte) (uint64, bool, error) { return s.m.Get(h) }
func (s *swissPM) Set(h [32]byte, p uint64) error       { return s.m.Set(h, p) }
func (s *swissPM) Delete(h [32]byte) (bool, error)      { return s.m.Delete(h) }
func (s *swissPM) Count() uint64                        { return s.m.Count() }
func (s *swissPM) ForEach(fn func(uint64))              { s.m.ForEach(fn) }
func (s *swissPM) Close() error                         { return s.m.Close() }
func (s *swissPM) SetConsistencyHash(h [32]byte) error  { return s.m.SetConsistencyHash(h) }
func (s *swissPM) BeginBatch(n uint64) (PositionMapBatch, error) {
	b, err := s.m.BeginBatch(n)
	if err != nil {
		return nil, err
	}
	return &swissPMBatch{b: b}, nil
}

type swissPMBatch struct{ b swisstable.Batch }

func (s *swissPMBatch) Insert(h [32]byte, p uint64) error { return s.b.Insert(h, p) }

func newPositionMap(ctrlPath, slotsPath string, expectedEntries uint64, consistencyHash [32]byte, dataFile *cachedRWS, posMask uint64) (PositionMap, bool, error) {
	m, needsRebuild, err := swisstable.NewSwissPositionMap(ctrlPath, slotsPath, expectedEntries, consistencyHash, dataFile, posMask)
	if err != nil {
		return nil, false, err
	}
	return &swissPM{m: m}, needsRebuild, nil
}
