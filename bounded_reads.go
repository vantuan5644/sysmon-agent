package main

import (
	"fmt"
	"sync"
	"time"
)

// boundedReads runs blocking reads in the background, at most one outstanding
// per key, so a caller can wait for them up to a deadline and walk away.
//
// It exists for reads that reach a device which may not answer for a long
// time and cannot be cancelled once issued: a syscall blocked in a driver holds
// its goroutine and OS thread until the device responds. Keeping one read per
// key means a device that hangs strands one goroutine, never one per caller.
// A caller that gives up gets the last value that key ever produced instead,
// so a busy device keeps reporting what it reported a moment ago.
type boundedReads[T any] struct {
	mu      sync.Mutex
	pending map[string]*boundedRead[T]
	last    map[string]*boundedRead[T]
}

type boundedRead[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// start returns key's outstanding read, starting one if none is running.
func (b *boundedReads[T]) start(key string, read func() (T, error)) *boundedRead[T] {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.pending[key]; ok {
		return r
	}
	if b.pending == nil {
		b.pending = map[string]*boundedRead[T]{}
		b.last = map[string]*boundedRead[T]{}
	}
	r := &boundedRead[T]{done: make(chan struct{})}
	b.pending[key] = r
	go func() {
		defer func() {
			// Nothing above this goroutine recovers a panic.
			if recovered := recover(); recovered != nil {
				r.err = fmt.Errorf("read panicked: %v", recovered)
			}
			b.mu.Lock()
			delete(b.pending, key)
			b.last[key] = r
			b.mu.Unlock()
			close(r.done)
		}()
		r.value, r.err = read()
	}()
	return r
}

// result waits for r until deadline. If r is still running it falls back to
// the last read of key that finished; ok is false when there is none.
func (b *boundedReads[T]) result(key string, r *boundedRead[T], deadline time.Time) (value T, ok bool, err error) {
	select {
	case <-r.done:
		return r.value, true, r.err
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-r.done:
		return r.value, true, r.err
	case <-timer.C:
	}
	b.mu.Lock()
	last, ok := b.last[key]
	b.mu.Unlock()
	if !ok {
		return value, false, nil
	}
	return last.value, true, last.err
}
