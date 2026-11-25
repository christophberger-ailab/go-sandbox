package image

import (
	"fmt"
	"sync"
	"testing"
)

func TestSetIndexConcurrent(t *testing.T) {
	// Separate Store values model separate gsb processes: only the flock
	// is shared between them.
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &Store{Dir: dir}
			if err := s.setIndex(fmt.Sprintf("img:%d", i), fmt.Sprintf("%064d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	idx, err := (&Store{Dir: dir}).index()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 50 {
		t.Fatalf("index has %d entries, want 50", len(idx))
	}
}
