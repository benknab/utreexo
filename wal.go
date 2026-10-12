package utreexo

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// wal coordinates crash-safe writes across cachedRWS instances and a
// deletedBitmap. It uses a write-ahead journal to ensure atomicity:
// either all buffered writes are applied to the underlying files, or
// none are.
//
// Journal format (all little-endian):
//
//	totalLen  uint64   (byte length of entries block)
//	bestHash  [32]byte (consistency hash, e.g. best block hash)
//	[entries] ...      (totalLen bytes)
//	checksum  uint32   (CRC32-IEEE of totalLen + bestHash + entries)
//
// Each entry:
//
//	fileIdx   uint8    (which underlying file)
//	offset    int64    (byte offset in that file)
//	dataLen   uint32   (length of data)
//	data      []byte   (the actual bytes)
//
// File indices in the journal:
//
//	0 = main hash file     (cachedRWS)
//	1 = block counts file  (cachedRWS)
//	2 = meta file          (cachedRWS)
//	    Bytes   0-31: recordMode (byte 0), zero-padded
//	    Bytes  32-63: numLeaves (bytes 32-39, LE), zero-padded
//	    Bytes  64-95: consistency hash (written by WAL.Flush)
//	    Bytes 96-127: generated leaves (bytes 96-103, LE), zero-padded
//	3 = deleted bitmap file (dirty words from deletedBitmap)
//
// Flush sequence:
//  1. Stream entries from cachedRWS caches + dirty bitmap words
//  2. Add consistency hash entry (file 2, offset 64)
//  3. Write bestHash + entries + CRC32 checksum to journal
//  4. Sync journal
//  5. Apply entries to underlying files (including consistency hash)
//  6. Sync underlying files
//  7. Clear journal (write totalLen=0, sync)
//  8. Reset cachedRWS caches + clear bitmap dirty tracking
//
// Recovery (in newWAL):
//  1. Read journal; if valid checksum found, replay entries to underlying
//  2. Clear journal
//  3. Load bitmap from recovered underlying file
//
// The consistency hash is written to file 2 (metaFile) at offset 64,
// and can be read from there after recovery or normal startup.
type wal struct {
	journal    io.ReadWriteSeeker
	cached     [3]*cachedRWS // [0]=main, [1]=blockCounts, [2]=meta
	bitmap     *deletedBitmap
	bitmapFile forestFile
	onFlush    func([32]byte) error
}

const (
	journalHeaderSize   = 8  // uint64 totalLen
	journalHashSize     = 32 // [32]byte bestHash
	journalChecksumSize = 4  // uint32 CRC32
	entryHeaderSize     = 13 // 1 (fileIdx) + 8 (offset) + 4 (dataLen)
	journalMinSize      = journalHeaderSize + journalHashSize + journalChecksumSize
	journalBufferSize   = 64 << 10

	metaFileIdx    = 2  // journal fileIdx for the metadata file
	deletedFileIdx = 3  // journal fileIdx for the deleted bitmap file
	bestHashOffset = 64 // byte offset of the consistency hash in the metadata file

	// generatedLeavesOffset is the byte offset of the generated-leaves
	// count in the metadata file. See Forest.saveGeneratedLeaves for the
	// slot's semantics.
	generatedLeavesOffset = 96
)

// walFile represents an underlying file with its entry size and cache config.
type walFile struct {
	File          forestFile
	EntrySize     int   // 4, 8, or 32
	MaxCacheBytes int64 // 0 means use default (64MB)
}

// newWAL creates a wal coordinating writes across the given underlying files.
// bitmapFile is the deleted-positions bitmap (not wrapped in cachedRWS — its
// dirty words are tracked by the in-memory deletedBitmap instead).
// files must be exactly 3 walFiles: [0]=main, [1]=blockCounts, [2]=meta.
// After recovery the bitmap is loaded from the underlying file and accessible
// via Bitmap(). Use Cached(i) to get the cachedRWS for file i.
func newWAL(journal io.ReadWriteSeeker, bitmapFile forestFile, files ...walFile) (*wal, error) {
	if len(files) != 3 {
		return nil, fmt.Errorf("wal requires exactly 3 files, got %d", len(files))
	}

	// Build the underlying array used for journal recovery.
	// Indices match journal fileIdx: 0=main, 1=blockCounts, 2=meta, 3=bitmap.
	underlying := make([]forestFile, 4)
	for i, f := range files {
		underlying[i] = f.File
	}
	underlying[deletedFileIdx] = bitmapFile

	w := &wal{
		journal:    journal,
		bitmapFile: bitmapFile,
	}

	// Replay any committed journal entries before creating caches: each
	// cachedRWS snapshots the underlying file's size at construction, so
	// any recovery-time growth must already be reflected.
	if err := w.recoverFromJournal(underlying); err != nil {
		return nil, fmt.Errorf("wal recover: %w", err)
	}

	// Load bitmap from the (possibly recovered) underlying file.
	bitmapSize, err := fileSize(w.bitmapFile)
	if err != nil {
		return nil, fmt.Errorf("wal bitmap size: %w", err)
	}
	bitmap, err := loadDeletedBitmap(w.bitmapFile, bitmapSize)
	if err != nil {
		return nil, fmt.Errorf("wal load bitmap: %w", err)
	}
	w.bitmap = bitmap

	for i, f := range files {
		size, err := fileSize(f.File)
		if err != nil {
			return nil, fmt.Errorf("wal file %d size: %w", i, err)
		}
		c, err := newCachedRWS(f.File, f.EntrySize, f.MaxCacheBytes, size)
		if err != nil {
			return nil, fmt.Errorf("wal wrap file %d: %w", i, err)
		}
		w.cached[i] = c
	}

	return w, nil
}

// Cached returns the cachedRWS for the i-th file.
// Indices: 0=main, 1=blockCounts, 2=meta.
func (w *wal) Cached(i int) *cachedRWS {
	return w.cached[i]
}

// Bitmap returns the in-memory deleted bitmap loaded from the underlying file.
// The bitmap tracks dirty words which are serialized during Flush.
func (w *wal) Bitmap() *deletedBitmap {
	return w.bitmap
}

// SetOnFlush registers a callback that runs after each successful Flush.
// The callback receives the bestHash that was flushed.
func (w *wal) SetOnFlush(fn func([32]byte) error) {
	w.onFlush = fn
}

// Close releases resources held by the cached stores (e.g. mmap regions).
func (w *wal) Close() {
	for _, c := range w.cached {
		c.Close()
	}
}

// Flush atomically commits all cached writes through the journal.
// The bestHash is written to metaFile (file index 2) at offset 64.
func (w *wal) Flush(bestHash [32]byte) error {
	if err := w.writeJournal(bestHash); err != nil {
		return err
	}

	// Sync journal to ensure it's durable before touching underlying files.
	if err := syncFile(w.journal); err != nil {
		return fmt.Errorf("wal journal sync: %w", err)
	}

	// Apply directly from caches + bitmap to underlying files.
	if err := w.applyFromCaches(bestHash); err != nil {
		return fmt.Errorf("wal apply: %w", err)
	}

	// The bestHash write above went directly to the underlying metaFile,
	// bypassing its cache. Bump the cache's size tracking to cover it so
	// reads of the consistency hash in this session fall through to the
	// underlying file instead of short-circuiting to io.EOF.
	w.cached[metaFileIdx].bumpMaxWritten(bestHashOffset + journalHashSize)

	// Sync underlying files (including bitmap file).
	for i, c := range w.cached {
		if err := syncFile(c.underlying); err != nil {
			return fmt.Errorf("wal sync file %d: %w", i, err)
		}
	}
	if err := syncFile(w.bitmapFile); err != nil {
		return fmt.Errorf("wal sync bitmap file: %w", err)
	}

	// Clear journal.
	if err := w.clearJournal(); err != nil {
		return fmt.Errorf("wal clear journal: %w", err)
	}

	// Reset caches so baseSize reflects the new underlying state.
	for i, c := range w.cached {
		if err := c.resetAfterFlush(); err != nil {
			return fmt.Errorf("wal reset cache %d: %w", i, err)
		}
	}

	// Clear bitmap dirty tracking.
	w.bitmap.clearDirty()

	if w.onFlush != nil {
		if err := w.onFlush(bestHash); err != nil {
			return fmt.Errorf("wal onFlush: %w", err)
		}
	}

	return nil
}

// writeJournal preserves the journal format while using a fixed-size buffer.
// The checksum covers bytes as they are written; the journal must still be
// synced by the caller before applying any changes to the underlying files.
func (w *wal) writeJournal(bestHash [32]byte) error {
	size := uint64(entryHeaderSize + journalHashSize)
	for _, c := range w.cached {
		size += uint64(c.cache.count()) * uint64(entryHeaderSize+c.cache.entrySize())
	}
	size += uint64(len(w.bitmap.dirtyWords)) * (entryHeaderSize + 8)
	if _, err := w.journal.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("wal journal seek: %w", err)
	}
	crc := crc32.NewIEEE()
	buf := bufio.NewWriterSize(io.MultiWriter(w.journal, crc), journalBufferSize)
	var header [journalHeaderSize]byte
	binary.LittleEndian.PutUint64(header[:], size)
	if _, err := buf.Write(header[:]); err != nil {
		return fmt.Errorf("wal journal write header: %w", err)
	}
	if _, err := buf.Write(bestHash[:]); err != nil {
		return fmt.Errorf("wal journal write bestHash: %w", err)
	}

	var entryHeader [entryHeaderSize]byte
	var writeErr error
	writeEntry := func(fileIdx uint8, offset int64, data []byte) {
		if writeErr != nil {
			return
		}
		entryHeader[0] = fileIdx
		binary.LittleEndian.PutUint64(entryHeader[1:9], uint64(offset))
		binary.LittleEndian.PutUint32(entryHeader[9:], uint32(len(data)))
		if _, writeErr = buf.Write(entryHeader[:]); writeErr == nil {
			_, writeErr = buf.Write(data)
		}
	}

	for i, c := range w.cached {
		c.cache.forEach(func(offset int64, data []byte) {
			writeEntry(uint8(i), offset, data)
		})
	}
	w.bitmap.forEachDirty(func(offset int64, data []byte) {
		writeEntry(deletedFileIdx, offset, data)
	})
	writeEntry(metaFileIdx, bestHashOffset, bestHash[:])
	if writeErr != nil {
		return fmt.Errorf("wal journal write entries: %w", writeErr)
	}
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("wal journal flush buffer: %w", err)
	}

	var checksum [journalChecksumSize]byte
	binary.LittleEndian.PutUint32(checksum[:], crc.Sum32())
	n, err := w.journal.Write(checksum[:])
	if err != nil {
		return fmt.Errorf("wal journal write checksum: %w", err)
	}
	if n != len(checksum) {
		return fmt.Errorf("wal journal write checksum: %w", io.ErrShortWrite)
	}
	return nil
}

// applyFromCaches writes cached entries and dirty bitmap words directly to
// underlying files. Includes the bestHash entry.
func (w *wal) applyFromCaches(bestHash [32]byte) error {
	for i, c := range w.cached {
		var applyErr error
		c.cache.forEach(func(offset int64, data []byte) {
			if applyErr != nil {
				return
			}
			if _, err := c.underlying.WriteAt(data, offset); err != nil {
				applyErr = fmt.Errorf("file %d write at %d: %w", i, offset, err)
				return
			}
		})
		if applyErr != nil {
			return applyErr
		}
	}

	if err := w.bitmap.writeDirty(w.bitmapFile); err != nil {
		return err
	}

	// Write bestHash to metaFile at bestHashOffset.
	metaFile := w.cached[metaFileIdx].underlying
	if _, err := metaFile.WriteAt(bestHash[:], bestHashOffset); err != nil {
		return fmt.Errorf("metaFile write bestHash: %w", err)
	}

	return nil
}

// Discard drops all pending writes without committing.
// The in-memory bitmap is reloaded from the underlying file to revert
// any mutations from the discarded block.
func (w *wal) Discard() error {
	for _, c := range w.cached {
		c.Discard()
	}
	// Reload bitmap from the underlying file to revert in-memory mutations.
	// Unlike cachedRWS (which is a read-through cache over the underlying file),
	// the bitmap is fully in-memory, so we must explicitly restore it.
	bitmapSize, err := fileSize(w.bitmapFile)
	if err != nil {
		return fmt.Errorf("wal discard: bitmap size: %w", err)
	}
	bitmap, err := loadDeletedBitmap(w.bitmapFile, bitmapSize)
	if err != nil {
		return fmt.Errorf("wal discard: reload bitmap: %w", err)
	}
	w.bitmap = bitmap
	return nil
}

// FlushNeeded returns true if the main data file cache has exceeded its
// memory threshold. The block counts and meta caches are excluded because
// they are tiny and never need to trigger a flush on their own.
// Dirty bitmap words are not considered here because they are tiny (just word
// indices) and will be written to the journal when a flush is triggered by a
// cachedRWS overflow.
func (w *wal) FlushNeeded() bool {
	return w.cached[0].FlushNeeded()
}

var errInvalidJournal = errors.New("invalid journal entry")

// readJournalEntries validates the entry framing and optionally applies the
// writes. Both passes use bounded memory, including for large entry payloads
// in journals written by older versions.
func readJournalEntries(r io.Reader, remaining uint64, underlying []forestFile, apply bool) error {
	var header [entryHeaderSize]byte
	var data [journalBufferSize]byte
	for remaining > 0 {
		if remaining < entryHeaderSize {
			return fmt.Errorf("%w: trailing bytes", errInvalidJournal)
		}
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return err
		}
		remaining -= entryHeaderSize
		fileIdx := int(header[0])
		offset := binary.LittleEndian.Uint64(header[1:9])
		dataLen := uint64(binary.LittleEndian.Uint32(header[9:]))
		if fileIdx >= len(underlying) || offset > uint64(1<<63-1)-dataLen || dataLen > remaining {
			return fmt.Errorf("%w: file %d, offset %d, length %d", errInvalidJournal, fileIdx, offset, dataLen)
		}
		remaining -= dataLen
		for dataLen > 0 {
			chunk := data[:min(dataLen, uint64(len(data)))]
			if _, err := io.ReadFull(r, chunk); err != nil {
				return err
			}
			if apply {
				n, err := underlying[fileIdx].WriteAt(chunk, int64(offset))
				if err != nil {
					return err
				}
				if n != len(chunk) {
					return io.ErrShortWrite
				}
			}
			offset += uint64(len(chunk))
			dataLen -= uint64(len(chunk))
		}
	}
	return nil
}

// clearJournal writes a zero totalLen to indicate no pending transaction
// and truncates the file to reclaim disk space.
func (w *wal) clearJournal() error {
	if _, err := w.journal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var zero [journalHeaderSize]byte
	n, err := w.journal.Write(zero[:])
	if err != nil {
		return err
	}
	if n != len(zero) {
		return io.ErrShortWrite
	}
	// Truncate to reclaim disk space if the file supports it.
	if truncater, ok := w.journal.(interface{ Truncate(size int64) error }); ok {
		if err := truncater.Truncate(journalHeaderSize); err != nil {
			return err
		}
	}
	return syncFile(w.journal)
}

// recoverFromJournal replays any committed journal entries to the
// underlying files. Called once during newWAL, before cachedRWS creation.
func (w *wal) recoverFromJournal(underlying []forestFile) error {
	// Get journal size.
	size, err := w.journal.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}

	if size < int64(journalMinSize) {
		return nil // No valid journal.
	}

	// Read totalLen.
	if _, err := w.journal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(w.journal, journalBufferSize)
	var header [journalHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	totalLen := binary.LittleEndian.Uint64(header[:])

	if totalLen == 0 {
		return nil // No pending transaction.
	}

	// Bound totalLen to the file size and int64 limits to avoid overflow/OOM.
	if totalLen > uint64(size)-journalMinSize {
		return w.clearJournal()
	}
	// Read bestHash.
	var bestHash [journalHashSize]byte
	if _, err := io.ReadFull(r, bestHash[:]); err != nil {
		return err
	}

	// Verify the entire record before changing any underlying file. Read
	// errors leave the journal intact so recovery can be retried.
	crc := crc32.NewIEEE()
	crc.Write(header[:])
	crc.Write(bestHash[:])
	if err := readJournalEntries(io.TeeReader(r, crc), totalLen, underlying, false); err != nil {
		if errors.Is(err, errInvalidJournal) {
			return w.clearJournal()
		}
		return err
	}

	// Read stored checksum.
	var storedChecksum uint32
	if err := binary.Read(r, binary.LittleEndian, &storedChecksum); err != nil {
		return err
	}

	if crc.Sum32() != storedChecksum {
		// Corrupt journal; discard.
		return w.clearJournal()
	}

	// Replay in a second streaming pass. On failure keep the committed
	// journal: these offset writes are idempotent on the next recovery.
	if _, err := w.journal.Seek(journalHeaderSize+journalHashSize, io.SeekStart); err != nil {
		return err
	}
	r.Reset(w.journal)
	if err := readJournalEntries(r, totalLen, underlying, true); err != nil {
		return err
	}

	// Sync underlying files after replay.
	for _, u := range underlying {
		if err := syncFile(u); err != nil {
			return err
		}
	}

	return w.clearJournal()
}

// syncFile calls Sync() on f if it supports it (e.g. *os.File).
// For implementations without Sync (e.g. memFile), this is a no-op.
func syncFile(f any) error {
	if syncer, ok := f.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}
	return nil
}
