//go:build linux && amd64

package utreexo

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// BenchmarkForestReadScheduling measures the production scheduler with real
// independent disk reads. Skew represents a batch containing both cached and
// cold source regions. Only this fixture's file cache is evicted.
func BenchmarkForestReadScheduling(b *testing.B) {
	if os.Getenv("UTREEXO_COLD_BENCH") != "1" {
		b.Skip("set UTREEXO_COLD_BENCH=1 on disk-backed scratch storage")
	}
	f, err := os.Create(filepath.Join(b.TempDir(), "reads"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	const pages = 32768
	buf := make([]byte, 1<<20)
	for range pages / 256 {
		if _, err := f.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		b.Fatal(err)
	}
	for _, skew := range []bool{false, true} {
		b.Run(fmt.Sprintf("skew=%t", skew), func(b *testing.B) {
			offsets := make([]int64, 5000)
			rng := rand.New(rand.NewSource(42))
			for i := range offsets {
				offsets[i] = int64(rng.Intn(pages)) * 4096
				if skew && i < len(offsets)/2 {
					offsets[i] = 0
				}
			}
			forest := &Forest{proofReadWorkers: 4}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				// This benchmark is amd64-only because of the fadvise ABI.
				_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, 4, 0, 0)
				if errno != 0 {
					b.Fatal(errno)
				}
				if _, err := f.ReadAt(buf[:8], 0); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				err := forest.readProofRanges(len(offsets), func(start, end int) error {
					var data [8]byte
					for i := start; i < end; i++ {
						if _, err := f.ReadAt(data[:], offsets[i]); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
