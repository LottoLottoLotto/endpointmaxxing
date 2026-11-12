package lock

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWithExcludesOtherHoldersOfTheSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	// Each With opens the file anew, as separate processes would: the lock is per open file.
	var wg sync.WaitGroup
	inside, most := 0, 0
	var mu sync.Mutex
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := With(path, func() error {
				mu.Lock()
				inside++
				most = max(most, inside)
				mu.Unlock()
				for range 1000 {
					_ = os.Getpid()
				}
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d holders were inside the lock at once", most)
	}
}

func TestTryFailsWhileHeldAndSucceedsAfterUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	a, b := open(), open()
	if err := Try(a); err != nil {
		t.Fatalf("first Try: %v", err)
	}
	if err := Try(b); err == nil {
		t.Fatal("second Try succeeded while the lock was held")
	}
	Unlock(a)
	if err := Try(b); err != nil {
		t.Fatalf("Try after Unlock: %v", err)
	}
}
