package utreexo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Undo changes the mmap position index immediately but leaves forest data in
// the WAL cache. A crash before that WAL commits must recover the old index.
func TestForestUnflushedUndoRecovery(t *testing.T) {
	path := t.TempDir()
	f, err := OpenForest(path)
	require.NoError(t, err)
	adds := []Leaf{{Hash: Hash{1}}, {Hash: Hash{2}}, {Hash: Hash{3}}}
	hashes := []Hash{{1}, {2}, {3}}
	tip := [32]byte{42}
	require.NoError(t, f.Modify(adds, nil, Proof{}))
	require.NoError(t, f.Flush(tip))
	roots := f.GetRoots()
	require.NoError(t, f.Undo(hashes, Proof{}, nil, nil))
	require.NoError(t, f.CloseWithoutFlush())
	f, err = OpenForest(path)
	require.NoError(t, err)
	defer f.Close(tip)
	require.Equal(t, uint64(3), f.GetNumLeaves())
	require.Equal(t, roots, f.GetRoots())
	proof, err := f.Prove(hashes)
	require.NoError(t, err)
	require.NoError(t, f.Verify(hashes, proof, false))
}
