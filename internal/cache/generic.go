package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Loader is the seam between the services and the cache implementation.
// *Store satisfies it; an asynchronous loader (queue + worker, returning a
// pending state) could satisfy it later without touching the handlers.
type Loader interface {
	Load(ctx context.Context, key Key, fetch FetchFunc, opt LoadOptions) (Entry, error)
}

// PartialResult is implemented by cached types that can be degraded. A partial result is
// served normally but cached only for TTLs.Partial.
type PartialResult interface {
	PartialResult() bool
}

// Result is the typed outcome of GetOrLoad.
type Result[T any] struct {
	Data             T // zero unless Status == StatusOK
	Status           Status
	FetchedAt        time.Time
	Cached           bool
	RefreshThrottled bool
}

// GetOrLoad runs the read-through flow and (un)marshals the payload.
//
// It is a free function rather than a generic method: Loader is an interface
// and interfaces cannot declare generic methods.
//
// Results: status ok carries Data; status not_found carries no data and a nil
// error; a cached failure yields *CachedError; a fresh failure yields the
// fetch error itself.
func GetOrLoad[T any](ctx context.Context, l Loader, key Key, opt LoadOptions, fetch func(context.Context) (T, error)) (Result[T], error) {
	entry, err := l.Load(ctx, key, func(ctx context.Context) (Fetched, error) {
		v, err := fetch(ctx)
		if err != nil {
			return Fetched{}, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return Fetched{}, fmt.Errorf("cache: encode %s data: %w", key.Kind, err)
		}
		p, ok := any(v).(PartialResult)
		return Fetched{Data: b, Partial: ok && p.PartialResult()}, nil
	}, opt)

	res := Result[T]{Status: entry.Status, FetchedAt: entry.FetchedAt, Cached: entry.Cached, RefreshThrottled: entry.RefreshThrottled}
	if err != nil {
		return res, err
	}
	if entry.Status == StatusOK {
		if err := json.Unmarshal(entry.Data, &res.Data); err != nil {
			return res, fmt.Errorf("cache: decode %s data: %w", key.Kind, err)
		}
	}
	return res, nil
}
