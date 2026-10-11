//go:build unix

package swisstable

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrefetchAcrossResizeAndUpdates(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "hashes"))
	require.NoError(t, err)
	defer f.Close()
	const count = 4096
	for i := range count {
		h := testHashFromInt(i)
		_, err := f.Write(h[:])
		require.NoError(t, err)
	}
	m, _, err := NewSwissPositionMap(filepath.Join(dir, "ctrl"), filepath.Join(dir, "slots"), 16, [32]byte{}, fileHashSource{f}, testPositionMask)
	require.NoError(t, err)
	defer m.Close()
	for start := 0; start < count; start += 1024 {
		m.Prefetch(1024, func(i int) [32]byte { return testHashFromInt(start + i) })
		for i := start; i < start+1024; i++ {
			require.NoError(t, m.Set(testHashFromInt(i), uint64(i)))
		}
	}
	m.Prefetch(count, testHashFromInt)
	for i := range count {
		// Updates still replace an existing mapping, rather than insert duplicates.
		want := packPosIndex(uint64(i), 7)
		require.NoError(t, m.Set(testHashFromInt(i), want))
		got, ok, err := m.Get(testHashFromInt(i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, want, got)
	}
	require.Equal(t, uint64(count), m.Count())
}
