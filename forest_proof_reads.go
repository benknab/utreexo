package utreexo

import (
	"runtime"
	"sync"
)

const maxProofReadWorkers = 4

// readProofRanges overlaps independent mmap misses while the caller holds
// f.mu. Every worker is joined before returning, including on error, so no
// reads can escape into a subsequent mutation. Contiguous ranges preserve the
// serial path's first error and each worker owns its output slice elements.
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
	var wg sync.WaitGroup
	for worker := 1; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			errs[worker] = read(n*worker/workers, n*(worker+1)/workers)
		}(worker)
	}
	errs[0] = read(0, n/workers)
	wg.Wait()
	for _, err := range errs[:workers] {
		if err != nil {
			return err
		}
	}
	return nil
}
