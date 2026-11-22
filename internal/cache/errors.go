package cache

import (
	"errors"
	"time"
)

var (
	// ErrLockTimeout means the load lock could not be acquired before the load deadline.
	ErrLockTimeout = errors.New("cache: could not acquire load lock in time")
	// ErrStoreUnavailable means Redis could not be reached.
	ErrStoreUnavailable = errors.New("cache: redis unavailable")
	// ErrClosed means the store is shutting down and accepts no new loads.
	ErrClosed = errors.New("cache: store closed")
)

// CachedError is returned when a negative "error" entry is served from cache.
// The original typed error is gone; Code identifies it.
type CachedError struct {
	Code      string
	FetchedAt time.Time
}

func (e *CachedError) Error() string { return "cache: cached failure: " + e.Code }
