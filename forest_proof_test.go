package utreexo

import (
	"errors"
	"math/rand"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"
)

// Compare the full proof and TTL result with independent in-memory state after
// extensive move-ups, a persisted reopen, and undo. In particular, the caller's
// target order must survive sorting the scratch used to construct proof hashes.
func TestForestBatchProofReopen(t *testing.T) {
	path := t.TempDir()
	f, err := OpenForest(path)
	require.NoError(t, err)
	t.Cleanup(func() { f.CloseWithoutFlush() })
	p := NewAccumulator()
	const count = 4096
	adds := make([]Leaf, count)
	for i := range adds {
		adds[i].Hash = testHashFromInt(i + 1)
	}
	require.NoError(t, f.Modify(adds, nil, Proof{}))
	require.NoError(t, p.Modify(adds, nil, Proof{}))
	order := rand.New(rand.NewSource(23)).Perm(count)
	dels := make([]Hash, count*3/4)
	for i := range dels {
		dels[i] = adds[order[i]].Hash
	}
	proof, err := p.Prove(dels)
	require.NoError(t, err)
	require.NoError(t, p.Modify(nil, dels, proof))
	require.NoError(t, f.Modify(nil, dels, proof))
	tip := [32]byte{1}
	require.NoError(t, f.Close(tip))
	f, err = OpenForest(path)
	require.NoError(t, err)

	dels = make([]Hash, 512)
	indexes := make([]int32, len(dels))
	for i := range dels {
		index := order[count*3/4+i]
		dels[i] = adds[index].Hash
		indexes[i] = int32(index)
	}
	before := append([]Hash(nil), dels...)
	want, err := p.Prove(dels)
	require.NoError(t, err)
	got, err := f.Prove(dels)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, before, dels)
	roots := f.GetRoots()
	_, err = Verify(Stump{Roots: roots, NumLeaves: count}, dels, got)
	require.NoError(t, err)

	// A late unknown deletion must reject the whole mutation, including valid
	// earlier deletions and additions, before changing any roots or TTLs.
	unknown := testHashFromInt(count + 1)
	_, err = f.ModifyAndReturnTTLs([]Leaf{{Hash: unknown}}, append(append([]Hash(nil), dels...), unknown), Proof{})
	require.Error(t, err)
	require.Equal(t, uint64(count), f.GetNumLeaves())
	require.Equal(t, roots, f.GetRoots())
	again, err := f.Prove(dels)
	require.NoError(t, err)
	require.Equal(t, got, again)

	created, err := f.ModifyAndReturnTTLs(nil, dels, got)
	require.NoError(t, err)
	require.Equal(t, indexes, created)
	require.NoError(t, p.Modify(nil, dels, want))
	require.Equal(t, p.GetRoots(), f.GetRoots())
	require.NoError(t, f.Undo(nil, got, dels, roots))
	again, err = f.Prove(dels)
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestForestProveAndModify(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenForest(dir)
	require.NoError(t, err)
	t.Cleanup(func() { f.CloseWithoutFlush() })
	p := NewAccumulator()
	adds := make([]Leaf, 4096)
	for i := range adds {
		adds[i].Hash = testHashFromInt(i + 1)
	}
	require.NoError(t, f.Modify(adds, nil, Proof{}))
	require.NoError(t, p.Modify(adds, nil, Proof{}))
	order := rand.New(rand.NewSource(18)).Perm(len(adds))
	dels := make([]Hash, len(adds)*3/4)
	for i := range dels {
		dels[i] = adds[order[i]].Hash
	}
	want, err := p.Prove(dels)
	require.NoError(t, err)
	roots := f.GetRoots()
	injected := errors.New("undo storage failed")
	_, _, err = f.ProveAndModify(nil, dels, func(proof Proof) error {
		require.Equal(t, want, proof)
		return injected
	})
	require.ErrorIs(t, err, injected)
	require.Equal(t, uint64(len(adds)), f.GetNumLeaves())
	require.Equal(t, roots, f.GetRoots())

	called := false
	_, _, err = f.ProveAndModify(nil, append(append([]Hash(nil), dels...), testHashFromInt(len(adds)+1)), func(Proof) error {
		called = true
		return nil
	})
	require.Error(t, err)
	require.False(t, called)
	require.Equal(t, roots, f.GetRoots())

	// Retry the rejected operation, then persist and prove moved-up leaves.
	proof, indexes, err := f.ProveAndModify(nil, dels, nil)
	require.NoError(t, err)
	require.Equal(t, want, proof)
	for i, index := range indexes {
		require.Equal(t, int32(order[i]), index)
	}
	require.NoError(t, p.Modify(nil, dels, want))
	require.Equal(t, p.GetRoots(), f.GetRoots())
	require.NoError(t, f.Close([32]byte{1}))
	f, err = OpenForest(dir)
	require.NoError(t, err)
	dels = dels[:0]
	for _, i := range order[len(adds)*3/4 : len(adds)*7/8] {
		dels = append(dels, adds[i].Hash)
	}
	want, err = p.Prove(dels)
	require.NoError(t, err)
	roots = f.GetRoots()
	newAdds := []Leaf{{Hash: testHashFromInt(len(adds) + 2)}, {Hash: testHashFromInt(len(adds) + 3)}}
	proof, _, err = f.ProveAndModify(newAdds, dels, func(proof Proof) error {
		called = true
		require.Equal(t, want, proof)
		_, err := Verify(Stump{Roots: roots, NumLeaves: uint64(len(adds))}, dels, proof)
		return err
	})
	require.NoError(t, err)
	require.True(t, called)
	require.NoError(t, p.Modify(newAdds, dels, want))
	require.Equal(t, p.GetRoots(), f.GetRoots())
	require.NoError(t, f.Undo([]Hash{newAdds[0].Hash, newAdds[1].Hash}, proof, dels, roots))
	require.Equal(t, roots, f.GetRoots())
	again, err := f.Prove(dels)
	require.NoError(t, err)
	require.Equal(t, proof, again)

	require.NoError(t, f.EnterRecordMode())
	_, _, err = f.ProveAndModify(nil, dels, nil)
	require.Error(t, err)
}

func TestForestProofReadWorkers(t *testing.T) {
	f, err := OpenForest(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { f.CloseWithoutFlush() })
	p := NewAccumulator()
	adds := make([]Leaf, 4096)
	for i := range adds {
		adds[i].Hash = testHashFromInt(i + 1)
	}
	require.NoError(t, f.Modify(adds, nil, Proof{}))
	require.NoError(t, p.Modify(adds, nil, Proof{}))
	order := rand.New(rand.NewSource(123)).Perm(len(adds))
	dels := make([]Hash, len(adds)/2)
	for i := range dels {
		dels[i] = adds[order[i]].Hash
	}
	proof, err := p.Prove(dels)
	require.NoError(t, err)
	require.NoError(t, f.Modify(nil, dels, proof))
	require.NoError(t, p.Modify(nil, dels, proof))
	for i := range dels {
		dels[i] = adds[order[len(dels)+i]].Hash
	}
	want, err := p.Prove(dels)
	require.NoError(t, err)
	bad := append([]Hash(nil), dels...)
	bad[7], bad[len(bad)/2] = testHashFromInt(9999), testHashFromInt(10000)
	f.proofReadWorkers = 1
	_, wantErr := f.Prove(bad)
	require.Error(t, wantErr)
	for _, workers := range []int{1, 2, 4} {
		f.proofReadWorkers = workers
		got, err := f.Prove(dels)
		require.NoError(t, err)
		require.Equal(t, want.Targets, got.Targets)
		require.True(t, slices.Equal(want.Proof, got.Proof), "proof hashes differ")
		_, err = f.Prove(bad)
		require.EqualError(t, err, wantErr.Error())
	}

	// Multiple callers must not share a worker barrier or output storage.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				proof, err := f.Prove(dels)
				if err != nil || !slices.Equal(proof.Targets, want.Targets) || !slices.Equal(proof.Proof, want.Proof) {
					t.Errorf("concurrent proof mismatch: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestProofReadFailureJoinsWorkers(t *testing.T) {
	f := &Forest{proofReadWorkers: 4}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	firstErr, laterErr := errors.New("first range"), errors.New("later range")
	go func() {
		done <- f.readProofRanges(512, func(start, end int) error {
			switch start {
			case 0:
				return firstErr
			case 384:
				close(started)
				<-release
				return laterErr
			}
			return nil
		})
	}()
	<-started
	select {
	case err := <-done:
		close(release)
		t.Fatalf("returned before all readers joined: %v", err)
	default:
	}
	close(release)
	require.ErrorIs(t, <-done, firstErr)
}
