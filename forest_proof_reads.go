package utreexo

import (
	"runtime"
	"sync"
	"sync/atomic"
)

const maxProofReadWorkers = 4

// readProofRanges overlaps independent mmap misses while the caller holds
// f.mu. Every worker is joined before returning, including on error, so no
// reads can escape into a subsequent mutation. Small contiguous ranges let
// readers share uneven disk misses. Each worker owns its output elements;
// the lowest failing range preserves the serial path's first error.
func (f *Forest) readProofRanges(n int, read func(start, end int) error) error {
	workers := f.proofReadWorkers
	if workers == 0 {
		workers = 1
		if runtime.GOOS == "linux" {
			workers = min(maxProofReadWorkers, runtime.GOMAXPROCS(0))
		}
	}
	if n < 512 || workers < 2 {
		return read(0, n)
	}
	var errs [maxProofReadWorkers]error
	var failedAt [maxProofReadWorkers]int
	var next atomic.Uint64
	const chunkSize = 128
	work := func(worker int) {
		for {
			start := int(next.Add(chunkSize)) - chunkSize
			if start >= n {
				return
			}
			if err := read(start, min(start+chunkSize, n)); err != nil {
				errs[worker] = err
				failedAt[worker] = start
				return
			}
		}
	}
	var wg sync.WaitGroup
	for worker := 1; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			work(worker)
		}(worker)
	}
	work(0)
	wg.Wait()
	first := n
	var firstErr error
	for worker, err := range errs[:workers] {
		if err != nil && failedAt[worker] < first {
			first, firstErr = failedAt[worker], err
		}
	}
	return firstErr
}
