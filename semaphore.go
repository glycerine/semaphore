// Use of this source code is governed by the MIT license
// that can be found in the LICENSE file.

// Package semaphore provides an implementation of counting semaphore primitive with possibility to change limit
// after creation. This implementation is based on Compare-and-Swap primitive that in general case works faster
// than other golang channel-based semaphore implementations.
package semaphore // import "github.com/marusama/semaphore"

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Semaphore counting resizable semaphore synchronization primitive.
// Use the Semaphore to control access to a pool of resources.
// There is no guaranteed order, such as FIFO or LIFO, in which blocked goroutines enter the semaphore.
// A goroutine can enter the semaphore multiple times, by calling the Acquire or TryAcquire methods repeatedly.
// To release some or all of these entries, the goroutine can call the Release method
// that specifies the number of entries to be released.
// Change Semaphore capacity to lower or higher by SetLimit.
type Semaphore interface {
	// Acquire enters the semaphore a specified number of times, blocking only until ctx is done.
	// This operation can be cancelled via passed context (but it's allowed to pass ctx='nil').
	// Method can return error 'ErrCtxDone' if the passed context is cancelled,
	// but this behavior is not guaranteed and sometimes semaphore will still be acquired.
	Acquire(ctx context.Context, n int) error

	// TryAcquire acquires the semaphore without blocking.
	// On success, returns true. On failure, returns false and leaves the semaphore unchanged.
	TryAcquire(n int) bool

	// Release exits the semaphore a specified number of times and returns the previous count.
	Release(n int) int

	// SetLimit changes current semaphore limit in concurrent way.
	// It is allowed to change limit many times and it's safe to set limit higher or lower.
	SetLimit(limit int)

	// GetLimit returns current semaphore limit.
	GetLimit() int

	// GetCount returns current number of occupied entries in semaphore.
	GetCount() int
}

var (
	// ErrCtxDone predefined error - context is cancelled.
	ErrCtxDone = errors.New("ctx.Done()")
)

// semaphore impl Semaphore intf
type semaphore struct {
	//  state holds limit and count in one 64 bits unsigned integer
	//
	//                            state (64 bits)
	// +-----------------------------------------------------------------+
	//      limit (high 32 bits)                 count (low 32 bits)
	// +--------------------------------|--------------------------------+
	//
	state uint64

	// broadcast fields
	lock        sync.RWMutex
	broadcastCh *chan struct{}
}

// Initializes a new instance of the Semaphore, specifying the maximum number of concurrent entries.
func New(limit int) Semaphore {
	if limit <= 0 {
		panic("semaphore limit must be greater than 0")
	}
	broadcastCh := make(chan struct{})
	return &semaphore{
		state:       uint64(limit) << 32,
		broadcastCh: &broadcastCh,
	}
}

func (s *semaphore) Acquire(ctx context.Context, n int) error {
	if n <= 0 {
		panic("n must be positive number")
	}
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ErrCtxDone
			default:
			}
		}

		// get current semaphore count and limit
		state := atomic.LoadUint64(&s.state)
		count := state & 0xFFFFFFFF
		limit := state >> 32

		// new count
		newCount := count + uint64(n)

		if newCount <= limit {
			// try CAS
			if atomic.CompareAndSwapUint64(&s.state, state, limit<<32+newCount) {
				// success CAS
				return nil
			} else {
				// try again
				continue
			}
		} else {
			// semaphore is full
			s.lock.RLock()
			broadcastCh := *s.broadcastCh
			s.lock.RUnlock()

			if ctx != nil {
				select {
				case <-ctx.Done():
					return ErrCtxDone
				// waiting for broadcast signal
				case <-broadcastCh:
				}
			} else {
				select {
				// waiting for broadcast signal
				case <-broadcastCh:
				}
			}
		}
	}
}

func (s *semaphore) TryAcquire(n int) bool {
	if n <= 0 {
		panic("n must be positive number")
	}

	for {
		state := atomic.LoadUint64(&s.state)
		count := state & 0xFFFFFFFF
		limit := state >> 32
		newCount := count + uint64(n)
		if newCount <= limit {
			if atomic.CompareAndSwapUint64(&s.state, state, limit<<32+newCount) {
				return true
			} else {
				continue
			}
		} else {
			return false
		}
	}
}

func (s *semaphore) Release(n int) int {
	if n <= 0 {
		panic("n must be positive number")
	}
	for {
		state := atomic.LoadUint64(&s.state)
		count := state & 0xFFFFFFFF
		limit := state >> 32
		if count == 0 {
			panic("semaphore release without acquire")
		}
		newCount := count - uint64(n)
		if atomic.CompareAndSwapUint64(&s.state, state, state&0xFFFFFFFF00000000+newCount) {

			// notifying possible waiters only if there weren't free slots before
			if count >= limit {
				newBroadcastCh := make(chan struct{})
				s.lock.Lock()
				oldBroadcastCh := s.broadcastCh
				s.broadcastCh = &newBroadcastCh
				s.lock.Unlock()

				// send broadcast signal
				close(*oldBroadcastCh)
			}
			return int(count)
		}
	}
}

func (s *semaphore) SetLimit(limit int) {
	if limit <= 0 {
		panic("semaphore limit must be greater than 0")
	}
	for {
		state := atomic.LoadUint64(&s.state)
		if atomic.CompareAndSwapUint64(&s.state, state, uint64(limit)<<32+state&0xFFFFFFFF) {
			newBroadcastCh := make(chan struct{})
			s.lock.Lock()
			oldBroadcastCh := s.broadcastCh
			s.broadcastCh = &newBroadcastCh
			s.lock.Unlock()

			// send broadcast signal
			close(*oldBroadcastCh)
			return
		}
	}
}

func (s *semaphore) GetCount() int {
	state := atomic.LoadUint64(&s.state)
	return int(state & 0xFFFFFFFF)
}

func (s *semaphore) GetLimit() int {
	state := atomic.LoadUint64(&s.state)
	return int(state >> 32)
}
