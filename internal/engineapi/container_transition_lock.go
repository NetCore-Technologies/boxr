package engineapi

import "sync"

// containerTransitionLocks serializes lifecycle transitions for one container
// without blocking operations on unrelated containers. References include
// both the active holder and waiters, so entries can be safely reclaimed.
type containerTransitionLocks struct {
	mu    sync.Mutex
	locks map[string]*containerTransitionLock
}

type containerTransitionLock struct {
	mu   sync.Mutex
	refs int
}

func (locks *containerTransitionLocks) lock(id string) func() {
	locks.mu.Lock()
	if locks.locks == nil {
		locks.locks = make(map[string]*containerTransitionLock)
	}
	entry := locks.locks[id]
	if entry == nil {
		entry = &containerTransitionLock{}
		locks.locks[id] = entry
	}
	entry.refs++
	locks.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()

		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.locks, id)
		}
		locks.mu.Unlock()
	}
}
