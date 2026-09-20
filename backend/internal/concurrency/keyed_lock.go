// Package concurrency provides in-process serialization helpers that sit on
// top of database transactions. Device parameter updates and Cue approvals
// touch the same device-version evidence and must be mutually exclusive even
// before a database row lock would settle the race, so callers acquire ordered
// keys (ascending device IDs) and release every key they hold on failure.
package concurrency

import "sync"

// KeyedLocker hands out one non-reentrant mutex per string key. TryLock never
// blocks: it reports whether the key was acquired, which lets the losing side
// of a device-update/Cue-approval race fail immediately with a 409 instead of
// queueing behind a transaction it is no longer allowed to join.
type KeyedLocker struct {
	mu    sync.Mutex
	locks map[uint64]chan struct{}
}

func NewKeyedLocker() *KeyedLocker {
	return &KeyedLocker{locks: make(map[uint64]chan struct{})}
}

func (l *KeyedLocker) slot(key uint64) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot, ok := l.locks[key]
	if !ok {
		// A capacity-1 channel used as a mutex: one holder, fair FIFO waiters.
		slot = make(chan struct{}, 1)
		l.locks[key] = slot
	}
	return slot
}

// TryLock attempts to acquire key without blocking.
func (l *KeyedLocker) TryLock(key uint64) bool {
	select {
	case l.slot(key) <- struct{}{}:
		return true
	default:
		return false
	}
}

// Unlock releases a key previously acquired with TryLock.
func (l *KeyedLocker) Unlock(key uint64) {
	<-l.slot(key)
}

// AcquireOrdered takes every key in ascending order and returns the keys that
// must be released by the caller. If one key is busy, all keys already taken
// in this call are released and ok is false. Callers must pass a de-duplicated
// slice; AcquireOrdered sorts defensively so lock order is globally identical.
func (l *KeyedLocker) AcquireOrdered(keys []uint64) (acquired []uint64, ok bool) {
	unique := make([]uint64, 0, len(keys))
	seen := make(map[uint64]struct{}, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	for i := 1; i < len(unique); i++ {
		for j := i; j > 0 && unique[j-1] > unique[j]; j-- {
			unique[j-1], unique[j] = unique[j], unique[j-1]
		}
	}
	for index, key := range unique {
		if l.TryLock(key) {
			acquired = append(acquired, key)
			continue
		}
		for taken := index - 1; taken >= 0; taken-- {
			l.Unlock(unique[taken])
		}
		return nil, false
	}
	return unique, true
}

// ReleaseOrdered frees every key returned by AcquireOrdered. The slice order is
// irrelevant because every key is an independent mutex, but reversing keeps the
// acquisition/discharge symmetry familiar from ordered resource locking.
func (l *KeyedLocker) ReleaseOrdered(keys []uint64) {
	for index := len(keys) - 1; index >= 0; index-- {
		l.Unlock(keys[index])
	}
}
