package swisstable

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
)

// Retain the page-touching loads even though the values are only hints.
var prefetchSink atomic.Uint64

// Prefetch warms initial insertion groups and their hash candidates without modifying the table. The
// caller must exclude mutations until this method returns. All readers join
// before return; a later resize merely makes the hints ineffective.
func (m *SwissPositionMap) Prefetch(n int, hashAt func(int) [32]byte) {
	// These independent hint reads are dominated by cold page latency. Keep
	// their bound separate from proof readers, which also do shared-cache work.
	workers := min(8, runtime.GOMAXPROCS(0))
	if n < 512 || workers < 2 {
		return
	}
	var wg sync.WaitGroup
	read := func(worker int) {
		var sum uint64
		for i := n * worker / workers; i < n*(worker+1)/workers; i++ {
			hash := hashAt(i)
			h1, h2 := splitHash(hash)
			base := (h1 % m.numGroups) * groupSize
			// The header can make a group straddle a page boundary.
			sum += uint64(m.ctrl[base]) + uint64(m.ctrl[base+groupSize-1])
			sum += m.getSlot(base) + m.getSlot(base+groupSize-1)
			// Matching fingerprints require a second random read in the forest.
			// Warm those pages while readers can overlap misses. Hints never
			// substitute for Set's verification or error handling.
			for matches := m.matchH2(base, h2); matches != 0; matches &= matches - 1 {
				slot := base + uint64(bits.TrailingZeros16(matches))
				if ok, _ := m.verifyHash(m.getSlot(slot), hash); ok {
					sum++
				}
			}
		}
		prefetchSink.Add(sum)
	}
	for worker := 1; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) { defer wg.Done(); read(worker) }(worker)
	}
	read(0)
	wg.Wait()
}
