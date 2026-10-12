//go:build linux && amd64

package swisstable

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Evict only the fixture's own files, never the host's page cache. Compare
// complete insert batches so hint overhead and remaining serial faults count.
func BenchmarkSwissPrefetchInsert(b *testing.B) {
	if os.Getenv("UTREEXO_COLD_BENCH") != "1" {
		b.Skip("set UTREEXO_COLD_BENCH=1 on disk-backed scratch storage")
	}
	const count, batch = 1 << 20, 5000
	dir := b.TempDir()
	file, err := os.Create(filepath.Join(dir, "hashes"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { file.Close() })
	for base := 0; base < count+batch; {
		buf := make([]byte, min(32768, count+batch-base)*32)
		for i := 0; i < len(buf)/32; i++ {
			h := testHashFromInt(base + i)
			copy(buf[i*32:], h[:])
		}
		if _, err := file.Write(buf); err != nil {
			b.Fatal(err)
		}
		base += len(buf) / 32
	}
	if err := file.Sync(); err != nil {
		b.Fatal(err)
	}
	source := newMmapHashSource(b, file, (count+batch)*32)
	m, _, err := NewSwissPositionMap(filepath.Join(dir, "ctrl"), filepath.Join(dir, "slots"), count+batch*2, [32]byte{}, source, testPositionMask)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { m.Close() })
	for i := range count {
		if err := m.Set(testHashFromInt(i), uint64(i)); err != nil {
			b.Fatal(err)
		}
	}
	keys := make([][32]byte, batch)
	for i := range keys {
		keys[i] = testHashFromInt(count + i)
	}
	for _, cold := range []bool{false, true} {
		b.Run(fmt.Sprintf("cold=%t", cold), func(b *testing.B) {
			b.ReportAllocs()
			var faults int64
			for range b.N {
				b.StopTimer()
				if cold {
					if err := m.SetConsistencyHash([32]byte{1}); err != nil {
						b.Fatal(err)
					}
					// Exclude the once-per-checkpoint dirty-header sync from insertion timing.
					if err := m.markDirty(); err != nil {
						b.Fatal(err)
					}
					for _, mapping := range []struct {
						file *os.File
						data []byte
					}{{file, source.data}, {m.ctrlFile, m.ctrlMmap}, {m.slotsFile, m.slotsMmap}} {
						if err := syscall.Madvise(mapping.data, syscall.MADV_DONTNEED); err != nil {
							b.Fatal(err)
						}
						_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64, mapping.file.Fd(), 0, 0, 4, 0, 0)
						if errno != 0 {
							b.Fatal(errno)
						}
					}
				}
				var before, after syscall.Rusage
				syscall.Getrusage(syscall.RUSAGE_SELF, &before)
				b.StartTimer()
				for start := 0; start < len(keys); start += 1024 {
					end := min(start+1024, len(keys))
					m.Prefetch(end-start, func(i int) [32]byte { return keys[start+i] })
					for i := start; i < end; i++ {
						if err := m.Set(keys[i], uint64(count+i)); err != nil {
							b.Fatal(err)
						}
					}
				}
				b.StopTimer()
				syscall.Getrusage(syscall.RUSAGE_SELF, &after)
				faults += after.Majflt - before.Majflt
				for _, key := range keys {
					if ok, err := m.Delete(key); err != nil || !ok {
						b.Fatalf("delete: %t %v", ok, err)
					}
				}
			}
			if cold && faults == 0 {
				b.Fatal("no cold faults; use disk-backed TMPDIR")
			}
			b.ReportMetric(float64(faults)/float64(b.N), "majfaults/op")
		})
	}
}
