// Package flock is an exclusive advisory file lock shared by every gh process
// on the machine. The operating system releases it if the process dies.
package flock

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Lock is a held lock.
type Lock struct{ f *os.File }

const poll = 25 * time.Millisecond

// Acquire blocks until it holds the lock on path, ctx ends, or timeout passes.
func Acquire(ctx context.Context, path string, timeout time.Duration) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return &Lock{f: f}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("timed out after %s waiting for %s", timeout, path)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Release unlocks and closes the lock file.
func (l *Lock) Release() {
	_ = unlock(l.f)
	_ = l.f.Close()
}
