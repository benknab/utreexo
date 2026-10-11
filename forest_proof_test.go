package utreexo

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// Compare the full proof and TTL result with independent in-memory state after
// extensive move-ups, a persisted reopen, and undo. In particular, the caller's
// target order must survive sorting the scratch used to construct proof hashes.
func TestForestBatchProofReopen(t *testing.T) {
	path := t.TempDir()
	f, err := OpenForest(path)
	require.NoError(t, err)
	t.Cleanup(func() { f.CloseWithoutFlush() })
	p := NewAccumulator()
	const count = 4096
	adds := make([]Leaf, count)
	for i := range adds {
		adds[i].Hash = testHashFromInt(i + 1)
	}
	require.NoError(t, f.Modify(adds, nil, Proof{}))
	require.NoError(t, p.Modify(adds, nil, Proof{}))
	order := rand.New(rand.NewSource(23)).Perm(count)
	dels := make([]Hash, count*3/4)
	for i := range dels {
		dels[i] = adds[order[i]].Hash
	}
	proof, err := p.Prove(dels)
	require.NoError(t, err)
	require.NoError(t, p.Modify(nil, dels, proof))
	require.NoError(t, f.Modify(nil, dels, proof))
	tip := [32]byte{1}
	require.NoError(t, f.Close(tip))
	f, err = OpenForest(path)
	require.NoError(t, err)

	dels = make([]Hash, 512)
	indexes := make([]int32, len(dels))
	for i := range dels {
		index := order[count*3/4+i]
		dels[i] = adds[index].Hash
		indexes[i] = int32(index)
	}
	before := append([]Hash(nil), dels...)
	want, err := p.Prove(dels)
	require.NoError(t, err)
	got, err := f.Prove(dels)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, before, dels)
	roots := f.GetRoots()
	_, err = Verify(Stump{Roots: roots, NumLeaves: count}, dels, got)
	require.NoError(t, err)

	// A late unknown deletion must reject the whole mutation, including valid
	// earlier deletions and additions, before changing any roots or TTLs.
	unknown := testHashFromInt(count + 1)
	_, err = f.ModifyAndReturnTTLs([]Leaf{{Hash: unknown}}, append(append([]Hash(nil), dels...), unknown), Proof{})
	require.Error(t, err)
	require.Equal(t, uint64(count), f.GetNumLeaves())
	require.Equal(t, roots, f.GetRoots())
	again, err := f.Prove(dels)
	require.NoError(t, err)
	require.Equal(t, got, again)

	created, err := f.ModifyAndReturnTTLs(nil, dels, got)
	require.NoError(t, err)
	require.Equal(t, indexes, created)
	require.NoError(t, p.Modify(nil, dels, want))
	require.Equal(t, p.GetRoots(), f.GetRoots())
	require.NoError(t, f.Undo(nil, got, dels, roots))
	again, err = f.Prove(dels)
	require.NoError(t, err)
	require.Equal(t, got, again)
}
