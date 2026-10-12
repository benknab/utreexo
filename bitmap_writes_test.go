package utreexo

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

type bitmapWriteRecorder struct {
	*memFile
	writes int
	short  bool
}

func (r *bitmapWriteRecorder) WriteAt(data []byte, offset int64) (int, error) {
	r.writes++
	if r.short {
		return len(data) - 1, nil
	}
	return r.memFile.WriteAt(data, offset)
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
