package utreexo

import (
	"math/bits"
	"sync"
)

const (
	cachePageSize = 4096 // 4KB per page, matches OS page size
	bitsPerWord   = 64   // bits in a uint64 bitmap word
)

// cachePage is a fixed-size page that stores cache entries as slots within a
// contiguous byte array. A bitmap tracks which slots are occupied, allowing
// direct lookup and insert by slot index without per-entry map overhead.
type cachePage struct {
	data    [cachePageSize]byte
	present []uint64 // bitmap: 1 bit per slot
	count   int
}

// isPresent returns true if the given slot has data in the bitmap.
func (pg *cachePage) isPresent(slot int) bool {
	return pg.present[slot/bitsPerWord]>>(uint(slot)%bitsPerWord)&1 != 0
}

// setPresent marks the given slot as occupied in the bitmap.
func (pg *cachePage) setPresent(slot int) {
	pg.present[slot/bitsPerWord] |= 1 << (uint(slot) % bitsPerWord)
}

// pageCacheStore implements cacheStore using fixed-size pages indexed by
// page number. Within each page, entries are accessed by direct array
// indexing with a bitmap tracking which slots are occupied.
//
// This amortizes the Go map overhead across many entries per page
// (e.g. 128 entries per page for 32-byte records) instead of one map
// entry per cached record.
type pageCacheStore struct {
	mu           sync.RWMutex
	pages        map[int64]*cachePage
	entrySize_   int
	slotsPerPage int
	wordsPerPage int // bitmap uint64 words needed per page
	maxBytes     int64
	pageBytes    int64 // backing data, bitmap, allocator and map allowance
	totalCount   int
}

// newPageCacheStore creates a pageCacheStore for entries of the given byte
// size. maxBytes sets the threshold for signaling that a flush is needed.
func newPageCacheStore(entrySize int, maxBytes int64) *pageCacheStore {
	if entrySize <= 0 || entrySize > cachePageSize {
		panic("pageCacheStore: entrySize must be in (0, 4096]")
	}
	slotsPerPage := cachePageSize / entrySize
	wordsPerPage := (slotsPerPage + bitsPerWord - 1) / bitsPerWord

	return &pageCacheStore{
		pages:        make(map[int64]*cachePage),
		entrySize_:   entrySize,
		slotsPerPage: slotsPerPage,
		wordsPerPage: wordsPerPage,
		maxBytes:     maxBytes,
		// A cachePage includes more than its 4096-byte data array and
		// rounds up to a larger Go allocator class. Include a 1KB allowance
		// for that object, plus bitmap storage and 64 bytes of map overhead.
		pageBytes: int64(cachePageSize + 1024 + wordsPerPage*8 + 64),
	}
}

func (p *pageCacheStore) entrySize() int { return p.entrySize_ }

// pageAndSlot converts a byte offset into the page number and the slot
// index within that page.
func (p *pageCacheStore) pageAndSlot(offset int64) (pageNum int64, slot int) {
	pageNum = offset / cachePageSize
	slot = int(offset%cachePageSize) / p.entrySize_
	return
}

// getPage returns the page for pageNum, allocating it if it doesn't exist.
// The caller must hold mu for writing.
func (p *pageCacheStore) getPage(pageNum int64) *cachePage {
	pg := p.pages[pageNum]
	if pg == nil {
		pg = &cachePage{
			present: make([]uint64, p.wordsPerPage),
		}
		p.pages[pageNum] = pg
	}
	return pg
}

// get returns the cached entry at the given byte offset, or false if not present.
// The returned slice aliases the page's internal storage.
func (p *pageCacheStore) get(offset int64) ([]byte, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pageNum, slot := p.pageAndSlot(offset)
	pg := p.pages[pageNum]
	if pg == nil || !pg.isPresent(slot) {
		return nil, false
	}
	start := slot * p.entrySize_
	return pg.data[start : start+p.entrySize_], true
}

// put writes data into the cache at the given byte offset, allocating a page if needed.
func (p *pageCacheStore) put(offset int64, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pageNum, slot := p.pageAndSlot(offset)
	pg := p.getPage(pageNum)
	start := slot * p.entrySize_
	copy(pg.data[start:start+p.entrySize_], data)
	if !pg.isPresent(slot) {
		pg.setPresent(slot)
		pg.count++
		p.totalCount++
	}
	return nil
}

// clear removes all cached entries and frees all pages.
func (p *pageCacheStore) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Replacing the map releases its backing tables as well as the pages.
	p.pages = make(map[int64]*cachePage)
	p.totalCount = 0
}

// forEach iterates over all occupied slots, calling fn with the byte offset and
// data for each cached entry.
func (p *pageCacheStore) forEach(fn func(offset int64, data []byte)) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for pageNum, pg := range p.pages {
		baseOffset := pageNum * cachePageSize
		for w, word := range pg.present {
			for word != 0 {
				bit := bits.TrailingZeros64(word)
				slot := w*bitsPerWord + bit
				start := slot * p.entrySize_
				fn(baseOffset+int64(start), pg.data[start:start+p.entrySize_])
				word &= word - 1 // clear lowest set bit
			}
		}
	}
}

// overflowed accounts for allocated pages rather than logical entry bytes.
// A single sparse write retains a complete page until the cache is cleared.
func (p *pageCacheStore) overflowed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.pages) != 0 && 512+int64(len(p.pages))*p.pageBytes > p.maxBytes
}

// count returns the total number of cached entries across all pages.
func (p *pageCacheStore) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.totalCount
}

// close is a no-op for the page-based cache (no external resources to release).
func (p *pageCacheStore) close() {}
