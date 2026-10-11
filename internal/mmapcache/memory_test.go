//go:build linux

package mmapcache

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Sparse writes must exhaust the budget based on backing pages, even when
// the payload in those pages is much smaller than the budget.
func TestSparseMemoryBudget(t *testing.T) {
	const budget = 1 << 20
	pageSize := os.Getpagesize()
	for _, entrySize := range []int{4, 8, 32} {
		s := newTestStore(t, entrySize, budget)
		data := make([]byte, entrySize)
		for i := 0; i <= budget/pageSize; i++ {
			require.NoError(t, s.Put(int64(i*pageSize), data))
		}
		require.True(t, s.Overflowed(), "entry size %d: more than 1 MiB of pages with only %d payload bytes", entrySize, s.Count()*entrySize)
		s.Clear()
		require.False(t, s.Overflowed())
		require.Zero(t, s.Count())
	}
}

// Dense writes and repeated overwrites should not be charged as separate pages.
func TestDenseMemoryBudget(t *testing.T) {
	const budget = 1 << 20
	s := newTestStore(t, 32, budget)
	var data [32]byte
	for pass := 0; pass < 3; pass++ {
		for offset := 0; offset < budget/4; offset += len(data) {
			require.NoError(t, s.Put(int64(offset), data[:]))
		}
		require.False(t, s.Overflowed())
	}
	require.Equal(t, budget/4/len(data), s.Count())
}

func TestConcurrentMemoryBudget(t *testing.T) {
	pageSize := os.Getpagesize()
	s := newTestStore(t, 32, int64(4*pageSize))
	var wg sync.WaitGroup
	for offset := 0; offset < pageSize; offset += 32 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			var data [32]byte
			if err := s.Put(int64(offset), data[:]); err != nil {
				t.Error(err)
			}
		}(offset)
	}
	wg.Wait()
	require.False(t, s.Overflowed(), "concurrent writes to one page must share its charge")
	var data [32]byte
	require.NoError(t, s.Put(int64(pageSize), data[:]))
	require.True(t, s.Overflowed())
}

func BenchmarkSparseMemoryBudget(b *testing.B) {
	s, err := New(32, 1<<20)
	require.NoError(b, err)
	b.Cleanup(s.Close)
	var data [32]byte
	pageSize := os.Getpagesize()
	var entries int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries = 0
		for !s.Overflowed() {
			if err := s.Put(int64(entries*pageSize), data[:]); err != nil {
				b.Fatal(err)
			}
			entries++
		}
		s.Clear()
	}
	b.ReportMetric(float64(entries*pageSize), "data-page-B/flush")
	b.ReportMetric(float64(entries*len(data)), "payload-B/flush")
}
