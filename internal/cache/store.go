package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redsync/redsync/v4"
	redsyncgoredis "github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"github.com/OWNER/siteinfo/internal/observability"
)

// Observer receives cache events. It is implemented by *observability.Metrics.
type Observer interface {
	CacheLookup(kind, outcome string)
	SingleflightShared(kind string)
	LockWait(kind, result string, d time.Duration)
	LoadFinished(kind, outcome string, d time.Duration)
	InflightLoads(delta int)
	CacheWriteFailed()
	Rejected(reason string)
}

type noopObserver struct{}

func (noopObserver) CacheLookup(string, string)                 {}
func (noopObserver) SingleflightShared(string)                  {}
func (noopObserver) LockWait(string, string, time.Duration)     {}
func (noopObserver) LoadFinished(string, string, time.Duration) {}
func (noopObserver) InflightLoads(int)                          {}
func (noopObserver) CacheWriteFailed()                          {}
func (noopObserver) Rejected(string)                            {}

// Verdict tells the store how to cache a failed fetch. The zero value (empty
// Status) means "do not cache, just return the error".
type Verdict struct {
	Status Status
	Code   string
}

// Classifier maps a fetch error to a Verdict.
type Classifier func(err error) Verdict

// TTLs are the entry lifetimes.
type TTLs struct {
	ImprintOK, BrandOK, NotFound, Error time.Duration
}

// For returns the TTL for a kind/status pair.
func (t TTLs) For(kind Kind, s Status) time.Duration {
	switch s {
	case StatusNotFound:
		return t.NotFound
	case StatusError:
		return t.Error
	default:
		if kind == KindBrand {
			return t.BrandOK
		}
		return t.ImprintOK
	}
}

// FetchFunc produces the JSON data for a cache miss. Returning an error that
// the Classifier maps to a Verdict negative-caches it.
type FetchFunc func(ctx context.Context) (json.RawMessage, error)

// LoadOptions are per-call settings.
type LoadOptions struct {
	// Refresh bypasses the cache read, subject to the per-key cooldown.
	Refresh bool
}

// Entry is the result of a Load.
type Entry struct {
	Envelope
	// Cached is true when no upstream load was performed for this call.
	Cached bool
	// RefreshThrottled is true when Refresh was requested but the cooldown was active.
	RefreshThrottled bool
}

// Store is the Redis backed read-through cache.
//
// Locking is a COST OPTIMIZATION, not a correctness guarantee. redsync runs
// against a single logical Redis (a cluster client addresses one master per
// slot), so a failover or an expired lock can let two pods load the same host.
// That produces one duplicate upstream call and an idempotent SET of equivalent
// data, which is acceptable. Do not build anything on the lock that needs
// mutual exclusion.
type Store struct {
	rdb redis.UniversalClient
	rs  *redsync.Redsync
	sf  singleflight.Group

	mu       sync.Mutex // guards closed and Add on inflight
	closed   bool
	inflight sync.WaitGroup

	loadTimeout   time.Duration
	lockMargin    time.Duration
	lockDelayMin  time.Duration
	lockDelayMax  time.Duration
	refreshCool   time.Duration
	writeTimeout  time.Duration
	unlockTimeout time.Duration
	ttls          TTLs
	maxLoads      int64         // 0 = unlimited
	loadLimiter   *rate.Limiter // nil = unlimited
	activeLoads   atomic.Int64
	classify      Classifier
	log           *zap.SugaredLogger
	obs           Observer
	now           func() time.Time
}

// Option configures optional Store settings.
type Option func(*Store)

// WithLoadTimeout bounds one load (lock wait plus upstream work). Default 40s.
func WithLoadTimeout(d time.Duration) Option { return func(s *Store) { s.loadTimeout = d } }

// WithLockMargin is added to the load timeout to get the lock expiry. Default 10s.
func WithLockMargin(d time.Duration) Option { return func(s *Store) { s.lockMargin = d } }

// WithLockRetryDelay sets the jittered pause between lock attempts. Default 50-100ms.
func WithLockRetryDelay(minDelay, maxDelay time.Duration) Option {
	return func(s *Store) { s.lockDelayMin, s.lockDelayMax = minDelay, maxDelay }
}

// WithLoadLimits protects the upstream (and its credits) from floods of cache
// misses, e.g. a client walking through random hostnames. maxConcurrent caps
// loads running at once in this pod; perSecond/burst is a token bucket on load
// starts in this pod. Zero disables the respective limit. Rejected loads fail
// fast with ErrOverloaded instead of queueing behind the crawler.
func WithLoadLimits(maxConcurrent int, perSecond float64, burst int) Option {
	return func(s *Store) {
		s.maxLoads = int64(max(maxConcurrent, 0))
		if perSecond > 0 {
			s.loadLimiter = rate.NewLimiter(rate.Limit(perSecond), max(burst, 1))
		}
	}
}

// WithTTLs sets entry lifetimes.
func WithTTLs(t TTLs) Option { return func(s *Store) { s.ttls = t } }

// WithRefreshCooldown sets the per-key cooldown between forced refreshes. Default 1h.
func WithRefreshCooldown(d time.Duration) Option { return func(s *Store) { s.refreshCool = d } }

// WithClassifier decides which fetch errors are negative-cached.
func WithClassifier(c Classifier) Option { return func(s *Store) { s.classify = c } }

// WithLogger sets the logger. Request-scoped fields come from the context (observability.Fields).
func WithLogger(l *zap.Logger) Option { return func(s *Store) { s.log = l.Sugar() } }

// WithObserver sets the metrics sink.
func WithObserver(o Observer) Option { return func(s *Store) { s.obs = o } }

// WithClock overrides time.Now for envelope timestamps.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// NewStore builds a Store on top of any go-redis client (cluster or single node).
func NewStore(rdb redis.UniversalClient, opts ...Option) *Store {
	s := &Store{
		rdb:           rdb,
		rs:            redsync.New(redsyncgoredis.NewPool(rdb)),
		loadTimeout:   40 * time.Second,
		lockMargin:    10 * time.Second,
		lockDelayMin:  50 * time.Millisecond,
		lockDelayMax:  100 * time.Millisecond,
		refreshCool:   time.Hour,
		writeTimeout:  3 * time.Second,
		unlockTimeout: 3 * time.Second,
		ttls: TTLs{
			ImprintOK: 30 * 24 * time.Hour, BrandOK: 30 * 24 * time.Hour,
			NotFound: 24 * time.Hour, Error: 5 * time.Minute,
		},
		classify: func(error) Verdict { return Verdict{} },
		log:      zap.NewNop().Sugar(),
		obs:      noopObserver{},
		now:      time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Ping checks Redis connectivity (used by the readiness probe).
func (s *Store) Ping(ctx context.Context) error {
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	return nil
}

// Close stops accepting new loads and waits for running ones until ctx is done.
// It does not close the Redis client; the owner of the client does that.
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { // ends when the (timeout-bounded) loads end
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cache: close: loads still running: %w", ctx.Err())
	}
}

func (s *Store) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.inflight.Add(1)
	return true
}

type loadResult struct {
	entry Entry
	err   error
}

// Load implements the read-through flow:
//
//  1. Redis GET, return on hit.
//  2. singleflight.DoChan per key; every caller selects on its own ctx.
//  3. The leader runs detached from the caller's cancellation with a load timeout.
//  4. Leader takes the redsync lock, 5. double-checks the cache, fetches, SETs, unlocks.
func (s *Store) Load(ctx context.Context, key Key, fetch FetchFunc, opt LoadOptions) (Entry, error) {
	kind := string(key.Kind)
	throttled := false
	if opt.Refresh {
		ok, err := s.rdb.SetNX(ctx, key.Cooldown(), "1", s.refreshCool).Result()
		if err != nil {
			return Entry{}, fmt.Errorf("%w: refresh cooldown: %w", ErrStoreUnavailable, err)
		}
		throttled = !ok
	}
	forceFresh := opt.Refresh && !throttled

	if !forceFresh {
		env, found, err := s.get(ctx, key)
		if err != nil {
			return Entry{}, err
		}
		if found {
			outcome := "hit"
			if throttled {
				outcome = "refresh_throttled"
			}
			s.obs.CacheLookup(kind, outcome)
			return s.cachedEntry(env, throttled)
		}
		s.obs.CacheLookup(kind, "miss")
	}

	ch := s.sf.DoChan(key.Cache(), func() (any, error) {
		return s.lead(ctx, key, fetch, forceFresh), nil
	})
	select {
	case r := <-ch:
		if r.Shared {
			s.obs.SingleflightShared(kind)
		}
		lr := r.Val.(loadResult)
		return lr.entry, lr.err
	case <-ctx.Done():
		return Entry{}, fmt.Errorf("cache: waiting for load of %s: %w", key.Host, ctx.Err())
	}
}

func (s *Store) cachedEntry(env Envelope, throttled bool) (Entry, error) {
	e := Entry{Envelope: env, Cached: true, RefreshThrottled: throttled}
	if env.Status == StatusError {
		return e, &CachedError{Code: env.ErrorCode, FetchedAt: env.FetchedAt}
	}
	return e, nil
}

func (s *Store) get(ctx context.Context, key Key) (Envelope, bool, error) {
	b, err := s.rdb.Get(ctx, key.Cache()).Bytes()
	if errors.Is(err, redis.Nil) {
		return Envelope{}, false, nil
	}
	if err != nil {
		return Envelope{}, false, fmt.Errorf("%w: get: %w", ErrStoreUnavailable, err)
	}
	env, err := DecodeEnvelope(b)
	if err != nil {
		// Unusable entry: drop it and treat as a miss so it gets rebuilt.
		s.log.Warnw("discarding undecodable cache entry", observability.Fields(ctx), "key", key.Cache(), "error", err)
		_ = s.rdb.Del(ctx, key.Cache()).Err()
		return Envelope{}, false, nil
	}
	return env, true, nil
}

// lead runs once per key and pod (singleflight leader).
func (s *Store) lead(parent context.Context, key Key, fetch FetchFunc, forceFresh bool) (res loadResult) {
	if !s.begin() {
		return loadResult{err: ErrClosed}
	}
	defer s.inflight.Done()

	// Admission control before any work: shed instead of queueing behind the crawler.
	if s.maxLoads > 0 {
		if s.activeLoads.Add(1) > s.maxLoads {
			s.activeLoads.Add(-1)
			s.obs.Rejected("load_concurrency")
			return loadResult{err: fmt.Errorf("%w: too many concurrent loads", ErrOverloaded)}
		}
		defer s.activeLoads.Add(-1)
	}
	if s.loadLimiter != nil && !s.loadLimiter.Allow() {
		s.obs.Rejected("load_rate")
		return loadResult{err: fmt.Errorf("%w: load rate limit", ErrOverloaded)}
	}
	s.obs.InflightLoads(1)
	defer s.obs.InflightLoads(-1)

	// The load must finish and fill the cache even if the leader's client is gone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.loadTimeout)
	defer cancel()

	kind := string(key.Kind)
	start := time.Now()
	outcome := "error"
	defer func() {
		if r := recover(); r != nil { // singleflight would re-panic on an unrecoverable goroutine
			s.log.Errorw("panic during cache load", observability.Fields(ctx), "key", key.Cache(),
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			res = loadResult{err: fmt.Errorf("cache: load panicked: %v", r)}
			outcome = "panic"
		}
		s.obs.LoadFinished(kind, outcome, time.Since(start))
	}()

	mu := s.rs.NewMutex(key.Lock(),
		redsync.WithExpiry(s.loadTimeout+s.lockMargin),
		redsync.WithTries(int(s.loadTimeout/s.lockDelayMin)+1), // bounded by ctx in practice
		redsync.WithRetryDelayFunc(s.lockDelay),
	)
	lockStart := time.Now()
	if err := mu.LockContext(ctx); err != nil {
		s.obs.LockWait(kind, "failed", time.Since(lockStart))
		if errors.Is(err, redsync.ErrFailed) || ctx.Err() != nil {
			outcome = "lock_timeout"
			return loadResult{err: fmt.Errorf("%w: %w", ErrLockTimeout, err)}
		}
		outcome = "unavailable"
		return loadResult{err: fmt.Errorf("%w: lock: %w", ErrStoreUnavailable, err)}
	}
	s.obs.LockWait(kind, "acquired", time.Since(lockStart))
	defer func() {
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), s.unlockTimeout)
		defer ucancel()
		if ok, err := mu.UnlockContext(uctx); err != nil || !ok {
			s.log.Warnw("releasing load lock failed", observability.Fields(ctx), "key", key.Cache(), "released", ok, "error", err)
		}
	}()

	// Another pod may have filled the cache while we waited for the lock.
	if !forceFresh {
		env, found, err := s.get(ctx, key)
		if err != nil {
			outcome = "unavailable"
			return loadResult{err: err}
		}
		if found {
			s.obs.CacheLookup(kind, "hit_after_lock")
			outcome = "hit_after_lock"
			e, err := s.cachedEntry(env, false)
			return loadResult{entry: e, err: err}
		}
	}

	data, ferr := fetch(ctx)
	env := Envelope{SchemaVersion: SchemaVersion, FetchedAt: s.now().UTC()}
	if ferr == nil {
		env.Status, env.Data = StatusOK, data
	} else {
		v := s.classify(ferr)
		if v.Status == "" {
			return loadResult{err: ferr} // transient / not site specific: never cached
		}
		env.Status, env.ErrorCode = v.Status, v.Code
	}
	outcome = string(env.Status)
	s.store(ctx, key, env)

	e := Entry{Envelope: env}
	if env.Status == StatusError {
		return loadResult{entry: e, err: ferr}
	}
	return loadResult{entry: e}
}

// store writes the envelope. Failures are logged, not returned: the freshly
// loaded result is still valid for the caller.
func (s *Store) store(ctx context.Context, key Key, env Envelope) {
	b, err := env.Encode()
	if err != nil {
		s.log.Errorw("encoding cache envelope failed", observability.Fields(ctx), "key", key.Cache(), "error", err)
		return
	}
	// Detached from the load deadline: an expensive result is worth a short grace period.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.writeTimeout)
	defer cancel()
	ttl := s.ttls.For(key.Kind, env.Status)
	if env.Status == StatusError {
		// NX: a failed forced refresh must not overwrite a good entry.
		err = s.rdb.SetNX(wctx, key.Cache(), b, ttl).Err()
	} else {
		err = s.rdb.Set(wctx, key.Cache(), b, ttl).Err()
	}
	if err != nil {
		s.obs.CacheWriteFailed()
		s.log.Errorw("writing cache entry failed", observability.Fields(ctx), "key", key.Cache(), "error", err)
	}
}

func (s *Store) lockDelay(int) time.Duration {
	span := s.lockDelayMax - s.lockDelayMin
	if span <= 0 {
		return s.lockDelayMin
	}
	return s.lockDelayMin + rand.N(span)
}
