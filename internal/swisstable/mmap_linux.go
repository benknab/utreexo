package swisstable

import "syscall"

// The position map can exceed RAM. Fault it on demand rather than populating
// both entire files at startup and evicting the forest and database caches.
const mmapFlags = syscall.MAP_SHARED

// Hash lookups and insertions touch uniformly distributed groups. Reading
// neighboring pages on each miss wastes I/O and cache space. This is only a
// performance hint; failure leaves the default paging behavior in place.
func adviseRandom(data []byte) {
	_ = syscall.Madvise(data, syscall.MADV_RANDOM)
}

// mmapAnon allocates an anonymous memory region of the given size via mmap.
// The returned slice is not tracked by Go's GC, so it must be freed with
// munmapAnon. MAP_NORESERVE avoids reserving swap space upfront.
func mmapAnon(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|syscall.MAP_NORESERVE)
}

// munmapAnon releases an anonymous mmap region allocated by mmapAnon.
func munmapAnon(b []byte) {
	syscall.Munmap(b)
}
