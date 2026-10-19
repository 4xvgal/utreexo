//go:build linux

package utreexo

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// ehUniformHash is a deterministic, well-distributed 32-byte key. The forest
// sim-chain hashes concentrate entropy in the low bytes, which makes the
// MSB-indexed EH directory degenerate; mainnet keys are sha256 and uniform.
func ehUniformHash(seed, i uint64) Hash {
	var h Hash
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

type ehBlock struct {
	adds []Leaf
	dels []Hash
}

func ehPlan() []ehBlock {
	const (
		perBlock = 2000
		delsPer  = 500
		blocks   = 4
	)
	plan := make([]ehBlock, 0, blocks)
	live := make([]Hash, 0, blocks*perBlock)
	rng := uint64(0x1234567)
	next := uint64(0)
	for b := 0; b < blocks; b++ {
		blk := ehBlock{}
		for k := 0; k < delsPer && len(live) > 0; k++ {
			rng = rng*6364136223846793005 + 1442695040888963407
			j := int((rng >> 33) % uint64(len(live)))
			blk.dels = append(blk.dels, live[j])
			live[j] = live[len(live)-1]
			live = live[:len(live)-1]
		}
		for k := 0; k < perBlock; k++ {
			h := ehUniformHash(0xE0E0, next)
			next++
			blk.adds = append(blk.adds, Leaf{Hash: h})
			live = append(live, h)
		}
		plan = append(plan, blk)
	}
	return plan
}

// TestEHPositionMapMatchesSwiss is the functional regression test for the
// bench-only "eh" position map: it drives the real Forest through
// ModifyAndReturnTTLs (adds + deletes) and requires the roots to match a
// Swiss-forest replay of the identical chain, across a close/reopen.
func TestEHPositionMapMatchesSwiss(t *testing.T) {
	if ehFactory == nil {
		t.Skip("eh position map not compiled in")
	}
	dirEH, dirSW := t.TempDir(), t.TempDir()
	open := func(dir string, eh bool) *Forest {
		opts := []ForestOption{ExpectedMaxLeaves(1 << 20), MaxCacheMemory(64 << 20)}
		if eh {
			opts = append(opts, PositionMapKind("eh"))
		}
		f, err := OpenForest(dir, opts...)
		require.NoError(t, err)
		return f
	}
	fe, fs := open(dirEH, true), open(dirSW, false)

	plan := ehPlan()
	for b, blk := range plan {
		_, _, err := fe.ModifyAndReturnTTLs(blk.adds, blk.dels, Proof{})
		require.NoErrorf(t, err, "eh block %d adds=%d dels=%d", b, len(blk.adds), len(blk.dels))
		_, _, err = fs.ModifyAndReturnTTLs(blk.adds, blk.dels, Proof{})
		require.NoErrorf(t, err, "swiss block %d", b)
		require.Equalf(t, fs.GetRoots(), fe.GetRoots(), "roots after block %d", b)
	}

	require.NoError(t, fe.Close([32]byte{0x42}))
	require.NoError(t, fs.Close([32]byte{0x42}))

	// Reopen: EH must reuse its on-disk table (consistency hash match) and
	// keep matching Swiss roots. A fresh, non-overlapping hash range is added.
	fe2, fs2 := open(dirEH, true), open(dirSW, false)
	defer fe2.Close([32]byte{0x42})
	defer fs2.Close([32]byte{0x42})

	blk := ehBlock{}
	next := uint64(1_000_000)
	for k := 0; k < 500; k++ {
		blk.adds = append(blk.adds, Leaf{Hash: ehUniformHash(0xE0E0, next)})
		next++
	}
	_, _, err := fe2.ModifyAndReturnTTLs(blk.adds, nil, Proof{})
	require.NoError(t, err)
	_, _, err = fs2.ModifyAndReturnTTLs(blk.adds, nil, Proof{})
	require.NoError(t, err)
	require.Equal(t, fs2.GetRoots(), fe2.GetRoots(), "roots after reopen block")
}
