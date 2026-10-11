//go:build linux

// Package mmapcache provides a cache backed by a single anonymous mmap region.
// The region covers the full data file address space but pages are
// demand-paged by the kernel, so only touched pages consume physical memory.
// A bitmap tracks which slots have cached data. A second-level (coarse)
// bitmap with one bit per 64 fine words and a third summary level let
// ForEach and Clear skip large empty regions cheaply. Atomic range tracking
// further bounds the scan without any mutex or dense list, making Put fully
// lock-free.
package mmapcache

import (
	"fmt"
	"math"
	"math/bits"
	"os"
	"sync/atomic"
	"unsafe"
)

const bitsPerWord = 64

// maxRegionSize is the virtual address space allocated for the data region.
// With MAP_ANONYMOUS|MAP_NORESERVE only touched pages consume physical memory,
// so this can be very large without cost. On 64-bit platforms the value is
// 1 TB (well within the 128 TB user-space limit); on 32-bit it is 1 GB.
//
// NOTE: 32-bit support is untested and may fail at runtime.
const maxRegionSize = 1 << (30 + 10*(^uint(0)>>63))

// Store is an mmap-backed cache for fixed-size entries.
//
// Concurrency:
//   - Get and Put on disjoint slots may run concurrently without external
//     synchronization. Put is lock-free in this case (atomic CAS on bitmap
//     words plus atomic min/max updates for range tracking).
//   - Put on the same slot, or Get concurrent with a Put that overwrites
//     that same slot, races on the data region (the byte copy is not
//     atomic and is not fenced for re-writes). Callers must serialize
//     same-slot access externally.
//   - ForEach, Clear, and Close are NOT safe to call concurrently with
//     any other method.
type Store struct {
	data    []byte // mmap'd data region (demand-paged)
	bitmap  []byte // mmap'd bitmap; tracks which slots have cached data
	bitmap2 []byte // mmap'd coarse bitmap; bit cw*64+cb means bitmap word cw*64+cb may be non-zero
	bitmap3 []byte // summary of non-empty coarse words; skips sparse row-layout gaps

	entrySize   int
	slots       int // total number of slots
	words       int // number of uint64 words in the fine bitmap
	coarseWords int // number of uint64 words in the coarse bitmap
	totalCount  atomic.Int64

	// Charge backing pages rather than payload bytes: a sparse 32-byte
	// write can allocate a whole OS page. The coarse bitmap already records
	// which 64-entry words are populated, so it also lets us count data-page
	// groups without another map or per-entry bookkeeping.
	maxBytes           int64
	memoryBytes        atomic.Int64
	wordsPerGroup      uint
	bytesPerGroup      int64
	metadataGroupBytes int64

	// Atomic range of bitmap word indices. ForEach and Clear scan only
	// [minWord, maxWord]. Initialized to empty (min > max).
	minWord atomic.Int64
	maxWord atomic.Int64
}

// New creates a Store for entries of the given byte size.
// maxBytes sets the threshold for signaling that a flush is needed.
func New(entrySize int, maxBytes int64) (*Store, error) {
	if entrySize <= 0 {
		return nil, fmt.Errorf("mmapcache: entrySize must be positive")
	}
	slots := maxRegionSize / entrySize
	words := (slots + bitsPerWord - 1) / bitsPerWord
	bitmapSize := words * 8
	if bitmapSize == 0 {
		bitmapSize = 8
	}
	coarseWords := (words + bitsPerWord - 1) / bitsPerWord
	coarseBitmapSize := coarseWords * 8
	if coarseBitmapSize == 0 {
		coarseBitmapSize = 8
	}

	data, err := mmapAnon(maxRegionSize)
	if err != nil {
		return nil, fmt.Errorf("mmapcache: mmap data %d bytes: %w", maxRegionSize, err)
	}
	bitmap, err := mmapAnon(bitmapSize)
	if err != nil {
		mmapRelease(data)
		return nil, fmt.Errorf("mmapcache: mmap bitmap %d bytes: %w", bitmapSize, err)
	}
	bitmap2, err := mmapAnon(coarseBitmapSize)
	if err != nil {
		mmapRelease(data)
		mmapRelease(bitmap)
		return nil, fmt.Errorf("mmapcache: mmap bitmap2 %d bytes: %w", coarseBitmapSize, err)
	}
	bitmap3, err := mmapAnon(max(8, ((coarseWords+bitsPerWord-1)/bitsPerWord)*8))
	if err != nil {
		mmapRelease(data)
		mmapRelease(bitmap)
		mmapRelease(bitmap2)
		return nil, fmt.Errorf("mmapcache: mmap bitmap3: %w", err)
	}

	s := &Store{
		data:        data,
		bitmap:      bitmap,
		bitmap2:     bitmap2,
		bitmap3:     bitmap3,
		entrySize:   entrySize,
		slots:       slots,
		words:       words,
		coarseWords: coarseWords,
		maxBytes:    maxBytes,
	}
	pageSize := int64(os.Getpagesize())
	wordBytes := int64(bitsPerWord) * int64(entrySize)
	s.wordsPerGroup = 1
	s.bytesPerGroup = ((wordBytes+pageSize-1)/pageSize + 1) * pageSize
	switch {
	case pageSize%wordBytes == 0:
		// A page can span more than one coarse word with small entries
		// and large OS pages. Charge each coarse word separately in that
		// case, conservatively overestimating rather than undercounting.
		s.wordsPerGroup = uint(min(pageSize/wordBytes, bitsPerWord))
		s.bytesPerGroup = pageSize
	case wordBytes%pageSize == 0:
		s.bytesPerGroup = wordBytes
	}
	// A coarse word covers 512 bytes of the fine bitmap plus its own
	// eight bytes. Charge a page for each, even when neighboring coarse
	// words share those pages. This small overestimate also avoids a
	// second tracking bitmap and its own memory-accounting overhead.
	s.metadataGroupBytes = 2 * pageSize
	// Initialize range sentinels to "empty".
	s.minWord.Store(math.MaxInt64)
	s.maxWord.Store(-1)
	return s, nil
}

// EntrySize returns the fixed size of each entry in bytes.
func (s *Store) EntrySize() int { return s.entrySize }

// bmWordPtr returns a *uint64 view of the w-th 8-byte word in the given
// bitmap, suitable for use with sync/atomic. The address is naturally
// aligned (w*8 is a multiple of 8 and the mmap base is page-aligned), which
// is required for the atomic ops to be sound.
func bmWordPtr(bm []byte, w int64) *uint64 {
	return (*uint64)(unsafe.Pointer(&bm[w*8]))
}

// atomicMinInt64 atomically updates *addr = min(*addr, val).
func atomicMinInt64(addr *atomic.Int64, val int64) {
	for {
		old := addr.Load()
		if val >= old {
			return
		}
		if addr.CompareAndSwap(old, val) {
			return
		}
	}
}

// atomicMaxInt64 atomically updates *addr = max(*addr, val).
func atomicMaxInt64(addr *atomic.Int64, val int64) {
	for {
		old := addr.Load()
		if val <= old {
			return
		}
		if addr.CompareAndSwap(old, val) {
			return
		}
	}
}

// forEachWord walks the coarse bitmap over the tracked range and invokes
// fn(w) for each fine-word index w whose coarse bit is set. Empty
// 4096-slot regions are skipped without inspecting their fine words.
//
// NOT safe for concurrent use with Put.
func (s *Store) forEachWord(fn func(w int64)) {
	s.forEachCoarseWord(func(cw int64) {
		cword := atomic.LoadUint64(bmWordPtr(s.bitmap2, cw))
		for cword != 0 {
			cbit := bits.TrailingZeros64(cword)
			cword &= cword - 1
			fn(cw*bitsPerWord + int64(cbit))
		}
	})
}

func (s *Store) forEachCoarseWord(fn func(cw int64)) {
	lo := s.minWord.Load()
	hi := s.maxWord.Load()
	if lo > hi {
		return
	}
	for tw := lo / (bitsPerWord * bitsPerWord); tw <= hi/(bitsPerWord*bitsPerWord); tw++ {
		word := atomic.LoadUint64(bmWordPtr(s.bitmap3, tw))
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			word &= word - 1
			fn(tw*bitsPerWord + int64(bit))
		}
	}
}

// Get retrieves the data at the given byte offset. Returns nil, false if
// the offset has no cached entry. The returned slice aliases internal storage.
//
// Lock-free: uses an atomic bitmap load. Safe to call concurrently with
// Put/Get on disjoint slots. Concurrent with a Put that overwrites this
// same slot, the returned bytes may be torn — see the Store doc.
func (s *Store) Get(offset int64) ([]byte, bool) {
	entrySize := int64(s.entrySize)
	slot := offset / entrySize
	w, bit := slot/bitsPerWord, uint(slot%bitsPerWord)

	if atomic.LoadUint64(bmWordPtr(s.bitmap, w))>>bit&1 == 0 {
		return nil, false
	}
	start := slot * entrySize
	return s.data[start : start+entrySize], true
}

// Put stores data at the given byte offset. len(data) must equal EntrySize().
//
// Lock-free with respect to other slots: uses atomic CAS on the bitmap and
// atomic min/max updates for range tracking. Same-slot concurrency (two
// Puts, or a Put overwriting while another goroutine Gets) races on the
// data region and must be serialized externally — see the Store doc.
func (s *Store) Put(offset int64, data []byte) error {
	if len(data) != s.entrySize {
		return fmt.Errorf("mmapcache: Put: len(data)=%d, want %d", len(data), s.entrySize)
	}
	entrySize := int64(s.entrySize)
	slot := offset / entrySize
	start := slot * entrySize

	// Data copy to disjoint slot — safe without lock. Must precede the
	// bit set so a concurrent Get that observes the bit also observes the
	// fully written data (CAS provides the required release barrier).
	copy(s.data[start:start+entrySize], data)

	w, bit := slot/bitsPerWord, uint(slot%bitsPerWord)
	mask := uint64(1) << bit

	bmPtr := bmWordPtr(s.bitmap, w)
	for {
		old := atomic.LoadUint64(bmPtr)
		if old&mask != 0 {
			return nil // already cached
		}
		if atomic.CompareAndSwapUint64(bmPtr, old, old|mask) {
			s.totalCount.Add(1)
			atomicMinInt64(&s.minWord, w)
			atomicMaxInt64(&s.maxWord, w)
			// If this is the first bit in the fine word, mark the
			// corresponding coarse bit so ForEach and Clear can skip
			// empty regions cheaply. Otherwise the coarse bit was
			// already set by a prior Put into this word.
			if old == 0 {
				cw := w / bitsPerWord
				cmask := uint64(1) << uint(w%bitsPerWord)
				coarsePtr := bmWordPtr(s.bitmap2, cw)
				for {
					oldC := atomic.LoadUint64(coarsePtr)
					if oldC&cmask != 0 {
						break
					}
					if atomic.CompareAndSwapUint64(coarsePtr, oldC, oldC|cmask) {
						groupStart := uint(w%bitsPerWord) / s.wordsPerGroup * s.wordsPerGroup
						groupMask := (^uint64(0) >> (bitsPerWord - s.wordsPerGroup)) << groupStart
						if oldC&groupMask == 0 {
							s.memoryBytes.Add(s.bytesPerGroup)
						}
						if oldC == 0 {
							s.memoryBytes.Add(s.metadataGroupBytes)
							top := bmWordPtr(s.bitmap3, cw/bitsPerWord)
							if atomic.OrUint64(top, uint64(1)<<uint(cw%bitsPerWord)) == 0 {
								// Conservatively charge a summary page per word.
								s.memoryBytes.Add(s.metadataGroupBytes / 2)
							}
						}
						break
					}
				}
			}
			return nil
		}
	}
}

// Clear removes all entries and releases the physical pages backing the
// data and bitmap regions. The virtual mappings remain allocated so the
// store can be reused without re-mapping. Releasing the bitmaps too bounds
// their physical memory over repeated flushes with different write offsets.
//
// NOT safe for concurrent use with Get/Put.
func (s *Store) Clear() {
	// Walk the coarse bitmap to find fine words with set bits and clear
	// them in a single pass. This avoids a linear scan over [minWord,
	// maxWord], which spans hundreds of millions of fine words for
	// sparse access patterns (e.g. the forest's row-based file layout)
	// and is prohibitively slow under -race.
	lo := s.minWord.Load()
	hi := s.maxWord.Load()
	if lo <= hi {
		s.forEachCoarseWord(func(cw int64) {
			cword := atomic.LoadUint64(bmWordPtr(s.bitmap2, cw))
			for cword != 0 {
				cbit := bits.TrailingZeros64(cword)
				cword &= cword - 1
				w := cw*bitsPerWord + int64(cbit)
				atomic.StoreUint64(bmWordPtr(s.bitmap, w), 0)
			}
			atomic.StoreUint64(bmWordPtr(s.bitmap2, cw), 0)
		})
		for tw := lo / (bitsPerWord * bitsPerWord); tw <= hi/(bitsPerWord*bitsPerWord); tw++ {
			atomic.StoreUint64(bmWordPtr(s.bitmap3, tw), 0)
		}
	}
	s.totalCount.Store(0)
	s.minWord.Store(math.MaxInt64)
	s.maxWord.Store(-1)
	released := true
	for _, region := range [][]byte{s.data, s.bitmap, s.bitmap2, s.bitmap3} {
		if err := madviseDontNeed(region); err != nil {
			released = false
		}
	}
	if released {
		s.memoryBytes.Store(0)
	}
	// If the kernel could not release a mapping, retain its charge. The
	// explicit bitmap clearing above still makes the logical reset valid.
	// Subsequent writes can overcount until a successful Clear, but cannot
	// hide retained pages from the flush threshold.
}

// Close releases all mmap regions back to the OS.
func (s *Store) Close() {
	if s.data != nil {
		mmapRelease(s.data)
		s.data = nil
	}
	if s.bitmap != nil {
		mmapRelease(s.bitmap)
		s.bitmap = nil
	}
	if s.bitmap2 != nil {
		mmapRelease(s.bitmap2)
		s.bitmap2 = nil
	}
	if s.bitmap3 != nil {
		mmapRelease(s.bitmap3)
		s.bitmap3 = nil
	}
	s.totalCount.Store(0)
	s.memoryBytes.Store(0)
}

// ForEach iterates over all cached entries, calling fn for each. Uses
// the summary bitmaps so empty 262144-slot regions are skipped without
// inspecting their fine words.
//
// NOT safe for concurrent use with Put.
func (s *Store) ForEach(fn func(offset int64, data []byte)) {
	entrySize := int64(s.entrySize)
	data := s.data
	s.forEachWord(func(w int64) {
		word := atomic.LoadUint64(bmWordPtr(s.bitmap, w))
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			slot := w*bitsPerWord + int64(bit)
			start := slot * entrySize
			fn(start, data[start:start+entrySize])
			word &= word - 1
		}
	})
}

// Overflowed reports whether the backing-page charge exceeds the memory
// budget. Swapping pages out does not reduce the charge. This is a flush
// threshold, not an allocation limit: callers check it between operations.
func (s *Store) Overflowed() bool { return s.memoryBytes.Load() > s.maxBytes }

// Count returns the total number of cached entries.
func (s *Store) Count() int { return int(s.totalCount.Load()) }
