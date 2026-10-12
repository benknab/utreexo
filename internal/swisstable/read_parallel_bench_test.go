//go:build linux && amd64

package swisstable

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// BenchmarkSwissBatchGet compares identical mmap lookups with one, two and
// four or eight readers. Cold trials evict only this benchmark's own scratch files.
// Opt in with UTREEXO_COLD_BENCH=1 and use a small fixed -benchtime (e.g. 3x).
func BenchmarkSwissBatchGet(b *testing.B) {
	if os.Getenv("UTREEXO_COLD_BENCH") != "1" {
		b.Skip("set UTREEXO_COLD_BENCH=1 for the scratch-file I/O benchmark")
	}
	dir := b.TempDir()
	file, err := os.Create(filepath.Join(dir, "hashes"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { file.Close() })
	const count = 1 << 20
	buf := make([]byte, 1<<20)
	for base := 0; base < count; base += len(buf) / 32 {
		for i := 0; i < len(buf)/32; i++ {
			h := testHashFromInt(base + i)
			copy(buf[i*32:], h[:])
		}
		if _, err := file.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
	if err := file.Sync(); err != nil {
		b.Fatal(err)
	}
	source := newMmapHashSource(b, file, count*32)
	m, _, err := NewSwissPositionMap(filepath.Join(dir, "ctrl"), filepath.Join(dir, "slots"),
		count*4, [32]byte{}, source, testPositionMask)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { m.Close() })
	for i := range count {
		if err := m.Set(testHashFromInt(i), uint64(i)); err != nil {
			b.Fatal(err)
		}
	}
	if err := m.SetConsistencyHash([32]byte{1}); err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(91))
	keys := make([][32]byte, 5000)
	want := make([]uint64, len(keys))
	for i := range keys {
		want[i] = uint64(rng.Intn(count))
		keys[i] = testHashFromInt(int(want[i]))
	}
	evict := func() {
		for _, mapping := range []struct {
			file *os.File
			data []byte
		}{{file, source.data}, {m.ctrlFile, m.ctrlMmap}, {m.slotsFile, m.slotsMmap}} {
			if err := syscall.Madvise(mapping.data, syscall.MADV_DONTNEED); err != nil {
				b.Fatal(err)
			}
			// Linux/amd64 fadvise64(fd, offset=0, length=0, DONTNEED=4).
			_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64, mapping.file.Fd(), 0, 0, 4, 0, 0)
			if errno != 0 {
				b.Fatal(errno)
			}
		}
	}
	for _, cold := range []bool{false, true} {
		for _, workers := range []int{1, 2, 4, 8} {
			b.Run(fmt.Sprintf("cold=%t/workers=%d", cold, workers), func(b *testing.B) {
				b.ReportAllocs()
				var before, after syscall.Rusage
				syscall.Getrusage(syscall.RUSAGE_SELF, &before)
				for range b.N {
					if cold {
						b.StopTimer()
						evict()
						b.StartTimer()
					}
					var errs [8]error
					var wg sync.WaitGroup
					read := func(worker int) {
						for i := len(keys) * worker / workers; i < len(keys)*(worker+1)/workers; i++ {
							got, found, err := m.Get(keys[i])
							if err != nil || !found || got != want[i] {
								errs[worker] = fmt.Errorf("lookup %d: got %d, %t, %v", i, got, found, err)
								return
							}
						}
					}
					for worker := 1; worker < workers; worker++ {
						wg.Add(1)
						go func(worker int) { defer wg.Done(); read(worker) }(worker)
					}
					read(0)
					wg.Wait()
					for _, err := range errs {
						if err != nil {
							b.Fatal(err)
						}
					}
				}
				syscall.Getrusage(syscall.RUSAGE_SELF, &after)
				if cold && after.Majflt == before.Majflt {
					b.Fatal("cold trial caused no disk faults; set TMPDIR to a disk-backed directory")
				}
				b.ReportMetric(float64(after.Majflt-before.Majflt)/float64(b.N), "majfaults/op")
			})
		}
	}
}
