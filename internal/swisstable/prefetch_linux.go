package swisstable

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// Retain the page-touching loads even though the values are only hints.
var prefetchSink atomic.Uint64

// Prefetch warms initial insertion groups without modifying the table. The
// caller must exclude mutations until this method returns. All readers join
// before return; a later resize merely makes the hints ineffective.
func (m *SwissPositionMap) Prefetch(n int, hashAt func(int) [32]byte) {
	workers := min(4, runtime.GOMAXPROCS(0))
	if n < 512 || workers < 2 {
		return
	}
	var wg sync.WaitGroup
	read := func(worker int) {
		var sum uint64
		for i := n * worker / workers; i < n*(worker+1)/workers; i++ {
			h1, _ := splitHash(hashAt(i))
			base := (h1 % m.numGroups) * groupSize
			// The header can make a group straddle a page boundary.
			sum += uint64(m.ctrl[base]) + uint64(m.ctrl[base+groupSize-1])
			sum += m.getSlot(base) + m.getSlot(base+groupSize-1)
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
