package server

import (
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
)

// This measurement needs neither a database nor listeners. Use -memprofile
// to inspect the retained template trees as well as parsing allocations.
func TestTemplatesMemory(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("template startup: heap delta %.2f MiB, heap %.2f MiB, system %.2f MiB", float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), float64(after.HeapAlloc)/(1<<20), float64(after.Sys)/(1<<20))
	runtime.KeepAlive(s)
}

func TestAllTemplatesParse(t *testing.T) {
	s, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for lang, pages := range s.pageSources {
		for page := range pages {
			if _, err := s.pageTemplate(lang, page); err != nil {
				t.Errorf("%s/%s: %v", lang, page, err)
			}
		}
	}
}

func TestTemplatesConcurrentFirstUse(t *testing.T) {
	s, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.pageTemplate("fr", "doc_api"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
