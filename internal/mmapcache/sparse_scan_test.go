//go:build linux

package mmapcache

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConcurrentSparseSummaryReuse(t *testing.T) {
	s := newTestStore(t, 32, 1<<30)
	for pass := range 3 {
		var workers sync.WaitGroup
		for worker := range 8 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				var data [32]byte
				data[0] = byte(pass + 1)
				for i := worker; i < 2048; i += 8 {
					// Exercise adjacent and distant coarse words concurrently.
					offset := int64(i/128)*(1<<32) + int64(i%128)*32 + int64(pass)*(1<<20)
					if err := s.Put(offset, data[:]); err != nil {
						t.Error(err)
					}
				}
			}()
		}
		workers.Wait()
		count, last := 0, int64(-1)
		s.ForEach(func(offset int64, data []byte) {
			require.Greater(t, offset, last)
			require.Equal(t, byte(pass+1), data[0])
			last = offset
			count++
		})
		require.Equal(t, 2048, count)
		s.Clear()
		s.ForEach(func(int64, []byte) { t.Fatal("entry survived Clear") })
		require.Zero(t, s.Count())
		require.Zero(t, s.memoryBytes.Load())
	}
}

func BenchmarkSparseRangeWalk(b *testing.B) {
	for _, stride := range []int64{32, 1 << 26} {
		b.Run(fmt.Sprintf("stride=%d", stride), func(b *testing.B) {
			s, err := New(32, 1<<30)
			require.NoError(b, err)
			defer s.Close()
			var data [32]byte
			for i := range 1000 {
				require.NoError(b, s.Put(int64(i)*stride, data[:]))
			}
			b.ResetTimer()
			for range b.N {
				count := 0
				s.ForEach(func(_ int64, _ []byte) { count++ })
				if count != 1000 {
					b.Fatal(count)
				}
			}
		})
	}
}
