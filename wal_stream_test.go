package utreexo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Bound individual writes to expose the former whole-journal allocation.
// A real file also exercises the normal Sync and Truncate behavior.
type boundedJournal struct {
	*os.File
	maxWrite int
	maxRead  int
}

func (j *boundedJournal) Read(p []byte) (int, error) {
	if j.maxRead > 0 && len(p) > j.maxRead {
		return 0, fmt.Errorf("unbounded journal read: %d bytes", len(p))
	}
	return j.File.Read(p)
}

func (j *boundedJournal) Write(p []byte) (int, error) {
	if len(p) > j.maxWrite {
		return 0, fmt.Errorf("unbounded journal write: %d bytes", len(p))
	}
	return j.File.Write(p)
}

func TestWALBoundedJournalWrites(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "journal")
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	j := &boundedJournal{File: f, maxWrite: 64 << 10}
	main := newMemFile()
	w, err := newWAL(j, newMemFile(),
		walFile{File: main, EntrySize: 32},
		walFile{File: newMemFile(), EntrySize: 4},
		walFile{File: newMemFile(), EntrySize: 32})
	require.NoError(t, err)
	t.Cleanup(w.Close)
	const entries = 6000
	for i := 0; i < entries; i++ {
		h := testHashFromInt(i + 1)
		require.NoError(t, w.Cached(0).PutHashAt(h, int64(i*32)))
	}
	require.NoError(t, w.Flush([32]byte{1}))
	for i := 0; i < entries; i++ {
		h := testHashFromInt(i + 1)
		require.Equal(t, h[:], main.data[i*32:(i+1)*32])
	}
	require.Zero(t, w.Cached(0).cache.count())
}

func TestWALStreamFormat(t *testing.T) {
	j := newMemFile()
	w, err := newWAL(j, newMemFile(),
		walFile{File: newMemFile(), EntrySize: 32},
		walFile{File: newMemFile(), EntrySize: 4},
		walFile{File: newMemFile(), EntrySize: 32})
	require.NoError(t, err)
	t.Cleanup(w.Close)
	require.NoError(t, w.Cached(0).PutHashAt([32]byte{1}, 64))
	require.NoError(t, w.Cached(1).PutUint32At(9, 4))
	require.NoError(t, w.Cached(2).PutHashAt([32]byte{2}, 32))
	w.Bitmap().set(17)
	bestHash := [32]byte{3}
	entries := w.serializeEntries(bestHash)
	expected := binary.LittleEndian.AppendUint64(nil, uint64(len(entries)))
	expected = append(expected, bestHash[:]...)
	expected = append(expected, entries...)
	expected = binary.LittleEndian.AppendUint32(expected, crc32.ChecksumIEEE(expected))
	require.NoError(t, w.writeJournal(bestHash))
	require.Equal(t, expected, j.data)
}

func TestWALBoundedJournalRecovery(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "journal")
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	j := &boundedJournal{File: f, maxWrite: 64 << 10, maxRead: 64 << 10}
	main, bitmap, counts, meta := newMemFile(), newMemFile(), newMemFile(), newMemFile()
	files := []walFile{{File: main, EntrySize: 32}, {File: counts, EntrySize: 4}, {File: meta, EntrySize: 32}}
	w, err := newWAL(j, bitmap, files...)
	require.NoError(t, err)
	t.Cleanup(w.Close)
	const entries = 6000
	for i := 0; i < entries; i++ {
		h := testHashFromInt(i + 1)
		require.NoError(t, w.Cached(0).PutHashAt(h, int64(i*32)))
	}
	bestHash := [32]byte{42}
	require.NoError(t, w.writeJournal(bestHash))
	require.NoError(t, j.Sync())
	// Simulate interruption after the commit, before any underlying writes.
	require.Empty(t, main.data)
	recovered, err := newWAL(j, bitmap, files...)
	require.NoError(t, err)
	t.Cleanup(recovered.Close)
	require.Len(t, main.data, entries*32)
	for i := 0; i < entries; i++ {
		h := testHashFromInt(i + 1)
		require.Equal(t, h[:], main.data[i*32:(i+1)*32])
	}
	require.Equal(t, bestHash[:], meta.data[64:96])
}

func TestWALRecoveryValidatesBeforeReplay(t *testing.T) {
	valid := serializeEntries([]journalEntry{{fileIdx: 0, data: []byte{42}}})
	for _, tc := range []struct {
		name string
		tail []byte
	}{
		{"trailing bytes", []byte{1}},
		{"truncated payload", serializeEntries([]journalEntry{{fileIdx: 0, data: []byte{1, 2}}})[:entryHeaderSize+1]},
		{"bad file", serializeEntries([]journalEntry{{fileIdx: 4, data: []byte{1}}})},
		{"negative offset", serializeEntries([]journalEntry{{fileIdx: 0, offset: -1, data: []byte{1}}})},
		{"overflowing offset", serializeEntries([]journalEntry{{fileIdx: 0, offset: 1<<63 - 1, data: []byte{1}}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := append(append([]byte(nil), valid...), tc.tail...)
			j := journalFixture(entries)
			main := newMemFile()
			w, err := newWAL(j, newMemFile(),
				walFile{File: main, EntrySize: 32},
				walFile{File: newMemFile(), EntrySize: 4},
				walFile{File: newMemFile(), EntrySize: 32})
			require.NoError(t, err)
			t.Cleanup(w.Close)
			require.Empty(t, main.data, "valid prefix must not be applied before validating the whole journal")
			require.Zero(t, binary.LittleEndian.Uint64(j.data))
		})
	}
}

func journalFixture(entries []byte) *memFile {
	data := binary.LittleEndian.AppendUint64(nil, uint64(len(entries)))
	data = append(data, make([]byte, journalHashSize)...)
	data = append(data, entries...)
	data = binary.LittleEndian.AppendUint32(data, crc32.ChecksumIEEE(data))
	return &memFile{data: data}
}

type readFaultJournal struct {
	*memFile
	err error
}

func (j *readFaultJournal) Read(p []byte) (int, error) {
	if j.err != nil {
		return 0, j.err
	}
	return j.memFile.Read(p)
}

func TestWALRecoveryReadFailureRetainsJournal(t *testing.T) {
	j := &readFaultJournal{memFile: journalFixture(serializeEntries([]journalEntry{{fileIdx: 0, data: []byte{42}}})), err: errJournalTest}
	original := append([]byte(nil), j.data...)
	main := newMemFile()
	files := []forestFile{main, newMemFile(), newMemFile(), newMemFile()}
	w := &wal{journal: j}
	require.ErrorIs(t, w.recoverFromJournal(files), errJournalTest)
	require.Equal(t, original, j.data)
	require.Empty(t, main.data)
	j.err = nil
	require.NoError(t, w.recoverFromJournal(files))
	require.Equal(t, []byte{42}, main.data)
}

type replayFaultFile struct {
	*memFile
	writes int
}

func (f *replayFaultFile) WriteAt(p []byte, off int64) (int, error) {
	f.writes++
	if f.writes == 2 {
		return len(p) - 1, nil
	}
	return f.memFile.WriteAt(p, off)
}

func TestWALRecoveryPartialReplay(t *testing.T) {
	// Include a payload larger than the streaming buffer for compatibility
	// with journals whose entries are not restricted to current cache sizes.
	payload := make([]byte, 100000)
	for i := range payload {
		payload[i] = byte(i)
	}
	j := journalFixture(serializeEntries([]journalEntry{{fileIdx: 0, data: payload}}))
	original := append([]byte(nil), j.data...)
	main := &replayFaultFile{memFile: newMemFile()}
	w := &wal{journal: j}
	files := []forestFile{main, newMemFile(), newMemFile(), newMemFile()}
	require.ErrorIs(t, w.recoverFromJournal(files), io.ErrShortWrite)
	require.NotEmpty(t, main.data)
	require.Equal(t, original, j.data, "failed replay must remain recoverable")
	require.NoError(t, w.recoverFromJournal(files))
	require.Equal(t, payload, main.data)
}

type faultJournal struct {
	*memFile
	writes    int
	failWrite int
	short     bool
	syncErr   error
}

var errJournalTest = errors.New("injected journal failure")

func (j *faultJournal) Write(p []byte) (int, error) {
	j.writes++
	if j.writes == j.failWrite {
		if j.short {
			return j.memFile.Write(p[:len(p)-1])
		}
		return 0, errJournalTest
	}
	return j.memFile.Write(p)
}

func (j *faultJournal) Sync() error { return j.syncErr }

func TestWALStreamFailurePreservesPendingData(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failWrite int
		short     bool
		syncErr   error
		wantErr   error
	}{
		{"buffer error", 1, false, nil, errJournalTest},
		{"buffer short write", 1, true, nil, io.ErrShortWrite},
		{"checksum error", 2, false, nil, errJournalTest},
		{"checksum short write", 2, true, nil, io.ErrShortWrite},
		{"sync error", 0, false, errJournalTest, errJournalTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &faultJournal{memFile: newMemFile(), failWrite: tc.failWrite, short: tc.short, syncErr: tc.syncErr}
			main := newMemFile()
			w, err := newWAL(j, newMemFile(),
				walFile{File: main, EntrySize: 32},
				walFile{File: newMemFile(), EntrySize: 4},
				walFile{File: newMemFile(), EntrySize: 32})
			require.NoError(t, err)
			t.Cleanup(w.Close)
			h := [32]byte{42}
			require.NoError(t, w.Cached(0).PutHashAt(h, 0))
			require.ErrorIs(t, w.Flush([32]byte{1}), tc.wantErr)
			require.Empty(t, main.data, "underlying data changed before a durable journal")
			require.Equal(t, 1, w.Cached(0).cache.count())
			got, err := w.Cached(0).HashAt(0)
			require.NoError(t, err)
			require.Equal(t, h, got)
			// A retry must preserve the buffered changes.
			j.failWrite = 0
			j.syncErr = nil
			require.NoError(t, w.Flush([32]byte{1}))
			require.Equal(t, h[:], main.data)
		})
	}
}
