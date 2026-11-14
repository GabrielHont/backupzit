package imaging

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

func TestFetchOrdered(t *testing.T) {
	ids := make([]repo.ID, 200)
	for i := range ids {
		if i%7 != 3 { // some blocks are empty (null)
			ids[i][0], ids[i][1] = byte(i), byte(i>>8)
			ids[i][2] = 1
		}
	}
	var inFlight, peak atomic.Int32
	load := func(ctx context.Context, id repo.ID) ([]byte, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
		return []byte{id[0], id[1]}, nil
	}
	var got []int
	err := fetchOrdered(context.Background(), 8, ids, load, func(i int, data []byte) error {
		if int(data[0])|int(data[1])<<8 != i {
			t.Fatalf("block %d got the data of %d", i, int(data[0])|int(data[1])<<8)
		}
		got = append(got, i)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for i := range ids {
		if !ids[i].IsNull() {
			if got[want] != i {
				t.Fatalf("order: position %d is block %d, want %d", want, got[want], i)
			}
			want++
		}
	}
	if len(got) != want {
		t.Fatalf("%d blocks used, want %d", len(got), want)
	}
	if peak.Load() < 2 || peak.Load() > 8 {
		t.Fatalf("parallel readers: %d", peak.Load())
	}

	// A failed download stops the restore with its error.
	boom := errors.New("storage unreachable")
	err = fetchOrdered(context.Background(), 4, ids, func(ctx context.Context, id repo.ID) ([]byte, error) {
		if id[0] == 50 {
			return nil, boom
		}
		return []byte{1}, nil
	}, func(int, []byte) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("error: %v", err)
	}
	// A failed write stops it too.
	err = fetchOrdered(context.Background(), 4, ids, load, func(i int, _ []byte) error {
		if i > 20 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("write error: %v", err)
	}
}
