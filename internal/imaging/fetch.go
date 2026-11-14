package imaging

import (
	"context"

	"github.com/max-zit/backupzit/internal/repo"
)

// restoreReaders is how many blocks a restore downloads at once (Unirea
// fork). One block at a time leaves the network waiting on each request's
// round trip to the storage; a few in parallel keep it busy.
const restoreReaders = 8

// fetchOrdered loads the non-null blobs of ids with n parallel readers and
// hands each to use in the order of ids, so the disk is written front to
// back. At most about 2n blocks are held in memory. The first error stops
// everything and is returned.
func fetchOrdered(ctx context.Context, n int, ids []repo.ID, load func(context.Context, repo.ID) ([]byte, error), use func(i int, data []byte) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		data []byte
		err  error
	}
	type future struct {
		i  int
		ch chan result
	}
	pending := make(chan future, 2*n)
	go func() {
		defer close(pending)
		sem := make(chan struct{}, n)
		for i, id := range ids {
			if id.IsNull() {
				continue
			}
			f := future{i, make(chan result, 1)}
			select {
			case pending <- f:
			case <-ctx.Done():
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				f.ch <- result{err: ctx.Err()}
				return
			}
			go func(id repo.ID, ch chan result) {
				defer func() { <-sem }()
				data, err := load(ctx, id)
				ch <- result{data, err}
			}(id, f.ch)
		}
	}()
	for f := range pending {
		r := <-f.ch
		if r.err != nil {
			return r.err
		}
		if err := use(f.i, r.data); err != nil {
			return err
		}
	}
	return ctx.Err()
}
