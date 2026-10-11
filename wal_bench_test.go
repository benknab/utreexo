package utreexo

import (
	"fmt"
	"testing"
)

// Discard output so the benchmark measures WAL overhead rather than the
// allocation and growth of the in-memory test files.
type discardJournal struct{ *memFile }

func (d discardJournal) Write(p []byte) (int, error) { return len(p), nil }

type discardForestFile struct{ *memFile }

func (d discardForestFile) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

func BenchmarkWALFlush(b *testing.B) {
	for _, entries := range []int{1000, 100000} {
		b.Run(fmt.Sprint(entries), func(b *testing.B) {
			file := discardForestFile{newMemFile()}
			w, err := newWAL(discardJournal{newMemFile()}, file,
				walFile{File: file, EntrySize: 32},
				walFile{File: file, EntrySize: 4},
				walFile{File: file, EntrySize: 32})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(w.Close)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				for j := 0; j < entries; j++ {
					if err := w.Cached(0).PutHashAt(Hash{1}, int64(j*32)); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if err := w.Flush([32]byte{1}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
