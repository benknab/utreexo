package utreexo

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

type bitmapWriteRecorder struct {
	*memFile
	writes  int
	short   bool
	shortAt int
}

func (r *bitmapWriteRecorder) WriteAt(data []byte, offset int64) (int, error) {
	r.writes++
	if r.short || r.writes == r.shortAt {
		return r.memFile.WriteAt(data[:len(data)/2], offset)
	}
	return r.memFile.WriteAt(data, offset)
}

func TestBitmapPageWriteRecovery(t *testing.T) {
	journal := newMemFile()
	bitmap := &bitmapWriteRecorder{memFile: newMemFile(), shortAt: 2}
	files := []walFile{
		{File: newMemFile(), EntrySize: 32},
		{File: newMemFile(), EntrySize: 4},
		{File: newMemFile(), EntrySize: 32},
	}
	w, err := newWAL(journal, bitmap, files...)
	require.NoError(t, err)
	for _, word := range []uint64{0, 17, 511, 512, 777, 1024} {
		w.Bitmap().set(word*64 + 3)
	}
	require.ErrorIs(t, w.Flush([32]byte{7}), io.ErrShortWrite)
	require.NotEmpty(t, journal.data)
	w.Close()
	// Reopen after a partly applied page: the ordinary per-word journal must
	// recover all intended changes, including words beyond the failed write.
	recovered, err := newWAL(journal, bitmap.memFile, files...)
	require.NoError(t, err)
	defer recovered.Close()
	for word := uint64(0); word <= 1024; word++ {
		want := uint64(0)
		switch word {
		case 0, 17, 511, 512, 777, 1024:
			want = 1 << 3
		}
		require.Equal(t, want, recovered.Bitmap().bits[word])
	}
	require.Len(t, bitmap.data, 1025*8)
	metadata := files[2].File.(*memFile)
	require.Equal(t, byte(7), metadata.data[bestHashOffset])
}

func TestBitmapPageWrites(t *testing.T) {
	b := newDeletedBitmap(2048 * 64)
	file := &bitmapWriteRecorder{memFile: newMemFile()}
	// Unchanged words between dirty ones must retain their persisted values.
	for i := range 1025 {
		b.bits[i] = uint64(i + 1)
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], b.bits[i])
		_, err := file.memFile.WriteAt(buf[:], int64(i)*8)
		require.NoError(t, err)
	}
	for _, word := range []uint64{0, 17, 511, 512, 1024} {
		b.set(word*64 + 63)
	}
	require.NoError(t, b.writeDirty(file))
	require.Equal(t, 3, file.writes)
	require.Len(t, file.data, 1025*8)
	for i := range 1025 {
		require.Equal(t, b.bits[i], binary.LittleEndian.Uint64(file.data[i*8:]))
	}
	// Dirty tracking belongs to the WAL commit, not the application helper.
	require.Len(t, b.dirtyWords, 5)
	file.short = true
	require.ErrorIs(t, b.writeDirty(file), io.ErrShortWrite)
	require.Len(t, b.dirtyWords, 5)
}
