package utreexo

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// benchmarkProofForest has a stable historical leaf set with most leaves
// deleted, like an archival Bitcoin forest. Seeded shuffles exercise moved-up
// leaves and shared ancestors rather than only fresh, adjacent positions.
func benchmarkProofForest(b *testing.B) (*Forest, []Hash) {
	b.Helper()
	const count = 1 << 18
	dir := b.TempDir()
	f, err := newForest(memCached(b, 32), memCached(b, 4), memCached(b, 32), nil,
		filepath.Join(dir, "ctrl"), filepath.Join(dir, "slots"), 20, count)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { f.positionMap.Close() })
	adds := make([]Leaf, count)
	for i := range adds {
		adds[i].Hash = testHashFromInt(i + 1)
	}
	if err := f.Modify(adds, nil, Proof{}); err != nil {
		b.Fatal(err)
	}
	order := rand.New(rand.NewSource(1)).Perm(count)
	dels := make([]Hash, count*3/4)
	for i := range dels {
		dels[i] = adds[order[i]].Hash
	}
	if err := f.Modify(nil, dels, Proof{}); err != nil {
		b.Fatal(err)
	}
	for _, file := range []*cachedRWS{f.file, f.blockCountsFile, f.metaFile} {
		if err := file.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	live := make([]Hash, count-len(dels))
	for i := range live {
		live[i] = adds[order[len(dels)+i]].Hash
	}
	return f, live
}

func BenchmarkForestProve(b *testing.B) {
	for _, targets := range []int{1, 1000, 5000} {
		b.Run(fmt.Sprintf("targets=%d", targets), func(b *testing.B) {
			f, live := benchmarkProofForest(b)
			dels := live[:targets]
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := f.Prove(dels); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkForestProofReaders(b *testing.B) {
	f, live := benchmarkProofForest(b)
	for _, workers := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			f.proofReadWorkers = workers
			b.ReportAllocs()
			for range b.N {
				if _, err := f.Prove(live[:5000]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkForestModifyTTLsUndo(b *testing.B) {
	f, live := benchmarkProofForest(b)
	dels := live[:5000]
	proof, err := f.Prove(dels)
	if err != nil {
		b.Fatal(err)
	}
	roots := f.GetRoots()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := f.ModifyAndReturnTTLs(nil, dels, proof); err != nil {
			b.Fatal(err)
		}
		if err := f.Undo(nil, proof, dels, roots); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkForestProveModifyUndo(b *testing.B) {
	for _, combined := range []bool{false, true} {
		b.Run(fmt.Sprintf("combined=%t", combined), func(b *testing.B) {
			f, live := benchmarkProofForest(b)
			dels := live[:5000]
			roots := f.GetRoots()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var proof Proof
				var err error
				if combined {
					proof, _, err = f.ProveAndModify(nil, dels, nil)
				} else {
					proof, err = f.Prove(dels)
					if err == nil {
						_, err = f.ModifyAndReturnTTLs(nil, dels, proof)
					}
				}
				if err != nil {
					b.Fatal(err)
				}
				if err := f.Undo(nil, proof, dels, roots); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkForestBlockUpdateUndo(b *testing.B) {
	f, live := benchmarkProofForest(b)
	dels := live[:5000]
	roots := f.GetRoots()
	adds := make([]Leaf, 5000)
	addHashes := make([]Hash, len(adds))
	for i := range adds {
		adds[i].Hash = testHashFromInt((1 << 18) + i + 1)
		addHashes[i] = adds[i].Hash
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		proof, _, err := f.ProveAndModify(adds, dels, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err := f.Undo(addHashes, proof, dels, roots); err != nil {
			b.Fatal(err)
		}
	}
}
