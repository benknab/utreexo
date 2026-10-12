package utreexo

import (
	"encoding/binary"
	"fmt"
	"io"
	"slices"
)

// writeDirty applies bitmap updates after the journal is durable. The bitmap
// contains authoritative values for unchanged words too, so nearby writes can
// share a page without reading the file. Never extend beyond the last dirty
// word: spare capacity in bits is not part of the persisted bitmap. Recovery
// still replays the original per-word journal records.
func (b *deletedBitmap) writeDirty(file io.WriterAt) error {
	words := make([]uint64, 0, len(b.dirtyWords))
	for word := range b.dirtyWords {
		if word < uint64(len(b.bits)) {
			words = append(words, word)
		}
	}
	slices.Sort(words)
	var buf [4096]byte
	for i := 0; i < len(words); {
		first := words[i]
		end := i + 1
		for end < len(words) && words[end]/512 == first/512 {
			end++
		}
		last := words[end-1]
		for word := first; word <= last; word++ {
			binary.LittleEndian.PutUint64(buf[(word-first)*8:], b.bits[word])
		}
		size := int(last-first+1) * 8
		n, err := file.WriteAt(buf[:size], int64(first)*8)
		if err == nil && n != size {
			err = io.ErrShortWrite
		}
		if err != nil {
			return fmt.Errorf("bitmap file write at %d: %w", first*8, err)
		}
		i = end
	}
	return nil
}
