package swisstable

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type failingPrefetchSource struct {
	calls atomic.Int64
}

func (s *failingPrefetchSource) HashAt(int64) ([32]byte, error) {
	s.calls.Add(1)
	return [32]byte{}, errors.New("candidate read failed")
}

func TestPrefetchCandidateErrorsRemainHints(t *testing.T) {
	dir := t.TempDir()
	file, err := os.Create(filepath.Join(dir, "hashes"))
	require.NoError(t, err)
	defer file.Close()
	hash := testHashFromInt(1)
	_, err = file.Write(hash[:])
	require.NoError(t, err)
	m, _, err := NewSwissPositionMap(filepath.Join(dir, "ctrl"), filepath.Join(dir, "slots"), 1024, [32]byte{}, fileHashSource{file}, testPositionMask)
	require.NoError(t, err)
	defer m.Close()
	require.NoError(t, m.Set(hash, 0))
	source := &failingPrefetchSource{}
	m.dataFile = source
	m.Prefetch(1024, func(int) [32]byte { return hash })
	// Hints may fail, but the real mutation must still verify and report it.
	require.ErrorContains(t, m.Set(hash, 7), "candidate read failed")
	require.Equal(t, uint64(1), m.Count())
	m.dataFile = fileHashSource{file}
	got, found, err := m.Get(hash)
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, got)
}
