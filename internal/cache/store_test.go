package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redsync/redsync/v4"
	redsyncgoredis "github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/redis/go-redis/v9"
)

var errUpstream = errors.New("upstream broke")

type recorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *recorder) inc(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[k]++
}
func (r *recorder) get(k string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[k]
}

func (r *recorder) CacheLookup(_, outcome string)              { r.inc("lookup:" + outcome) }
func (r *recorder) SingleflightShared(string)                  { r.inc("shared") }
func (r *recorder) LockWait(_, result string, _ time.Duration) { r.inc("lock:" + result) }
func (r *recorder) LoadFinished(_, outcome string, _ time.Duration) {
	r.inc("load:" + outcome)
}
func (r *recorder) InflightLoads(int)      {}
func (r *recorder) CacheWriteFailed()      { r.inc("writefail") }
func (r *recorder) Rejected(reason string) { r.inc("rejected:" + reason) }

type env struct {
	mr  *miniredis.Miniredis
	rdb *redis.Client
	rec *recorder
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return &env{mr: mr, rdb: rdb, rec: &recorder{}}
}

func (e *env) store(opts ...Option) *Store {
	base := []Option{
		WithObserver(e.rec),
		WithLoadTimeout(5 * time.Second),
		WithLockMargin(time.Second),
		WithLockRetryDelay(2*time.Millisecond, 5*time.Millisecond),
		WithClassifier(func(err error) Verdict {
			if errors.Is(err, errUpstream) {
				return Verdict{Status: StatusError, Code: "upstream_error"}
			}
			if errors.Is(err, errGone) {
				return Verdict{Status: StatusNotFound, Code: "gone"}
			}
			return Verdict{}
		}),
	}
	return NewStore(e.rdb, append(base, opts...)...)
}

var errGone = errors.New("gone")

var testKey = Key{Kind: KindImprint, Host: "example.de"}

func okFetch(counter *atomic.Int32, gate <-chan struct{}) FetchFunc {
	return func(ctx context.Context) (Fetched, error) {
		counter.Add(1)
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return Fetched{}, ctx.Err()
			}
		}
		return Fetched{Data: json.RawMessage(`{"v":1}`)}, nil
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestKeysUseHashTag(t *testing.T) {
	k := Key{Kind: KindBrand, Host: "xn--bcher-kva.de"}
	if k.Cache() != "brand:v2:{xn--bcher-kva.de}" || k.Lock() != "lock:brand:v2:{xn--bcher-kva.de}" {
		t.Fatalf("keys: %s %s", k.Cache(), k.Lock())
	}
	if !strings.HasSuffix(k.Cooldown(), "{xn--bcher-kva.de}") {
		t.Fatalf("cooldown key: %s", k.Cooldown())
	}
}

func TestEnvelopeRoundTripAndValidation(t *testing.T) {
	in := Envelope{SchemaVersion: SchemaVersion, Status: StatusOK, FetchedAt: time.Unix(1700000000, 0).UTC(), Data: json.RawMessage(`{"a":1}`)}
	b, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeEnvelope(b)
	if err != nil || out.Status != StatusOK || string(out.Data) != `{"a":1}` || !out.FetchedAt.Equal(in.FetchedAt) {
		t.Fatalf("roundtrip: %+v, %v", out, err)
	}
	for name, raw := range map[string]string{
		"garbage":        `nope`,
		"wrong version":  `{"schema_version":2,"status":"ok","data":{}}`,
		"unknown status": `{"schema_version":1,"status":"weird"}`,
		"ok no data":     `{"schema_version":1,"status":"ok"}`,
	} {
		if _, err := DecodeEnvelope([]byte(raw)); !errors.Is(err, ErrBadEnvelope) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, raw := range []string{`{"schema_version":1,"status":"not_found"}`, `{"schema_version":1,"status":"error","error_code":"x"}`} {
		if _, err := DecodeEnvelope([]byte(raw)); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// N callers, spread over two "pods" sharing one Redis, must trigger exactly one upstream load.
func TestConcurrentCallersTriggerOneUpstreamLoad(t *testing.T) {
	e := newEnv(t)
	podA, podB := e.store(), e.store()
	var calls atomic.Int32
	gate := make(chan struct{})
	fetch := okFetch(&calls, gate)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		s := podA
		if i%2 == 1 {
			s = podB
		}
		wg.Go(func() {
			entry, err := s.Load(context.Background(), testKey, fetch, LoadOptions{})
			if err == nil && string(entry.Data) != `{"v":1}` {
				err = errors.New("wrong data")
			}
			errs <- err
		})
	}
	waitFor(t, func() bool { return e.rec.get("lookup:miss") == n })
	waitFor(t, func() bool { return calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond) // let late callers reach singleflight / the lock
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", got)
	}
	if e.rec.get("shared") == 0 {
		t.Error("expected shared singleflight results to be counted")
	}
	// Everything is served from cache afterwards.
	entry, err := podA.Load(context.Background(), testKey, fetch, LoadOptions{})
	if err != nil || !entry.Cached || calls.Load() != 1 {
		t.Fatalf("after fill: %+v %v calls=%d", entry, err, calls.Load())
	}
}

func TestCallerCancelDoesNotFailOthersAndCacheStillFills(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	var calls atomic.Int32
	gate := make(chan struct{})
	fetch := okFetch(&calls, gate)

	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	go func() {
		_, err := s.Load(ctxA, testKey, fetch, LoadOptions{})
		errA <- err
	}()
	waitFor(t, func() bool { return calls.Load() == 1 })

	type out struct {
		entry Entry
		err   error
	}
	resB := make(chan out, 1)
	go func() {
		entry, err := s.Load(context.Background(), testKey, fetch, LoadOptions{})
		resB <- out{entry, err}
	}()
	waitFor(t, func() bool { return e.rec.get("lookup:miss") == 2 })

	cancelA() // the leader's client disconnects
	if err := <-errA; !errors.Is(err, context.Canceled) {
		t.Fatalf("A err = %v, want context.Canceled", err)
	}
	close(gate)
	b := <-resB
	if b.err != nil || string(b.entry.Data) != `{"v":1}` {
		t.Fatalf("B must not be affected by A's cancellation: %+v, %v", b.entry, b.err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if !e.mr.Exists(testKey.Cache()) {
		t.Fatal("cache must be filled")
	}
}

func TestLeaderCancelStillFillsCache(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	var calls atomic.Int32
	gate := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.Load(ctx, testKey, okFetch(&calls, gate), LoadOptions{})
		errCh <- err
	}()
	waitFor(t, func() bool { return calls.Load() == 1 })
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	close(gate)
	waitFor(t, func() bool { return e.mr.Exists(testKey.Cache()) })
}

func TestDoubleCheckAfterLock(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	other := redsync.New(redsyncgoredis.NewPool(e.rdb)).NewMutex(testKey.Lock(), redsync.WithExpiry(10*time.Second))
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	type out struct {
		entry Entry
		err   error
	}
	res := make(chan out, 1)
	go func() {
		entry, err := s.Load(context.Background(), testKey, okFetch(&calls, nil), LoadOptions{})
		res <- out{entry, err}
	}()
	waitFor(t, func() bool { return e.rec.get("lookup:miss") == 1 })

	// "Another pod" finishes its load while we wait for the lock.
	filled := Envelope{SchemaVersion: SchemaVersion, Status: StatusOK, FetchedAt: time.Now(), Data: json.RawMessage(`{"v":"other-pod"}`)}
	b, _ := filled.Encode()
	if err := e.rdb.Set(context.Background(), testKey.Cache(), b, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Unlock(); err != nil {
		t.Fatal(err)
	}

	r := <-res
	if r.err != nil || !r.entry.Cached || string(r.entry.Data) != `{"v":"other-pod"}` {
		t.Fatalf("got %+v, %v", r.entry, r.err)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream must not be called after the double-check hit, calls=%d", calls.Load())
	}
	if e.rec.get("lookup:hit_after_lock") != 1 {
		t.Error("double-check path was not taken")
	}
}

func TestLockFailureIsTypedError(t *testing.T) {
	e := newEnv(t)
	s := e.store(WithLoadTimeout(150*time.Millisecond), WithLockMargin(time.Second))
	other := redsync.New(redsyncgoredis.NewPool(e.rdb)).NewMutex(testKey.Lock(), redsync.WithExpiry(30*time.Second))
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	_, err := s.Load(context.Background(), testKey, okFetch(&calls, nil), LoadOptions{})
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if calls.Load() != 0 {
		t.Fatal("upstream must not be called without the lock")
	}
	if e.rec.get("lock:failed") != 1 {
		t.Error("lock failure metric missing")
	}
}

func TestLockIsReleasedAfterLoad(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	var calls atomic.Int32
	if _, err := s.Load(context.Background(), testKey, okFetch(&calls, nil), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.mr.Exists(testKey.Lock()) {
		t.Fatal("lock key must be gone after the load")
	}
	if ttl := e.mr.TTL(testKey.Cache()); ttl != 30*24*time.Hour {
		t.Fatalf("imprint ok ttl = %s", ttl)
	}
}

func TestNegativeCaching(t *testing.T) {
	t.Run("error is cached and replayed as CachedError", func(t *testing.T) {
		e := newEnv(t)
		s := e.store()
		var calls atomic.Int32
		fetch := func(context.Context) (Fetched, error) { calls.Add(1); return Fetched{}, errUpstream }

		_, err := s.Load(context.Background(), testKey, fetch, LoadOptions{})
		if !errors.Is(err, errUpstream) {
			t.Fatalf("fresh failure should return the original error, got %v", err)
		}
		_, err = s.Load(context.Background(), testKey, fetch, LoadOptions{})
		var ce *CachedError
		if !errors.As(err, &ce) || ce.Code != "upstream_error" {
			t.Fatalf("second call err = %v, want CachedError", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
		if ttl := e.mr.TTL(testKey.Cache()); ttl != 5*time.Minute {
			t.Fatalf("error ttl = %s", ttl)
		}
	})
	t.Run("not_found is a status, not an error", func(t *testing.T) {
		e := newEnv(t)
		s := e.store()
		var calls atomic.Int32
		fetch := func(context.Context) (Fetched, error) { calls.Add(1); return Fetched{}, errGone }
		for i, wantCached := range []bool{false, true} {
			entry, err := s.Load(context.Background(), testKey, fetch, LoadOptions{})
			if err != nil || entry.Status != StatusNotFound || entry.Cached != wantCached {
				t.Fatalf("call %d: %+v %v", i, entry, err)
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d", calls.Load())
		}
		if ttl := e.mr.TTL(testKey.Cache()); ttl != 24*time.Hour {
			t.Fatalf("not_found ttl = %s", ttl)
		}
	})
	t.Run("unclassified errors are never cached", func(t *testing.T) {
		e := newEnv(t)
		s := e.store()
		boom := errors.New("transient")
		for range 2 {
			if _, err := s.Load(context.Background(), testKey, func(context.Context) (Fetched, error) { return Fetched{}, boom }, LoadOptions{}); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
		}
		if e.mr.Exists(testKey.Cache()) {
			t.Fatal("transient error was cached")
		}
	})
}

func TestRefreshCooldown(t *testing.T) {
	e := newEnv(t)
	s := e.store(WithRefreshCooldown(time.Hour))
	var calls atomic.Int32
	fetch := okFetch(&calls, nil)
	ctx := context.Background()

	if _, err := s.Load(ctx, testKey, fetch, LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	entry, err := s.Load(ctx, testKey, fetch, LoadOptions{Refresh: true})
	if err != nil || entry.Cached || entry.RefreshThrottled || calls.Load() != 2 {
		t.Fatalf("first refresh: %+v %v calls=%d", entry, err, calls.Load())
	}
	entry, err = s.Load(ctx, testKey, fetch, LoadOptions{Refresh: true})
	if err != nil || !entry.Cached || !entry.RefreshThrottled || calls.Load() != 2 {
		t.Fatalf("throttled refresh: %+v %v calls=%d", entry, err, calls.Load())
	}
	e.mr.FastForward(time.Hour + time.Second)
	if _, err = s.Load(ctx, testKey, fetch, LoadOptions{Refresh: true}); err != nil || calls.Load() != 3 {
		t.Fatalf("refresh after cooldown: %v calls=%d", err, calls.Load())
	}
}

func TestFailedRefreshKeepsGoodEntry(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	var calls atomic.Int32
	ctx := context.Background()
	if _, err := s.Load(ctx, testKey, okFetch(&calls, nil), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load(ctx, testKey, func(context.Context) (Fetched, error) { return Fetched{}, errUpstream }, LoadOptions{Refresh: true})
	if !errors.Is(err, errUpstream) {
		t.Fatalf("err = %v", err)
	}
	entry, err := s.Load(ctx, testKey, okFetch(&calls, nil), LoadOptions{})
	if err != nil || entry.Status != StatusOK || !entry.Cached {
		t.Fatalf("good entry was overwritten: %+v %v", entry, err)
	}
}

func TestRefreshOnEmptyCacheStillLoads(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	var calls atomic.Int32
	// Cooldown already taken by someone else, nothing cached: must load normally.
	e.mr.Set(testKey.Cooldown(), "1")
	entry, err := s.Load(context.Background(), testKey, okFetch(&calls, nil), LoadOptions{Refresh: true})
	if err != nil || calls.Load() != 1 || string(entry.Data) != `{"v":1}` {
		t.Fatalf("%+v %v calls=%d", entry, err, calls.Load())
	}
}

func TestCorruptEntryIsRebuilt(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	e.mr.Set(testKey.Cache(), "not json")
	var calls atomic.Int32
	entry, err := s.Load(context.Background(), testKey, okFetch(&calls, nil), LoadOptions{})
	if err != nil || entry.Cached || calls.Load() != 1 {
		t.Fatalf("%+v %v calls=%d", entry, err, calls.Load())
	}
}

func TestCloseWaitsForInflightLoads(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	var calls atomic.Int32
	gate := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.Load(context.Background(), testKey, okFetch(&calls, gate), LoadOptions{})
		done <- err
	}()
	waitFor(t, func() bool { return calls.Load() == 1 })

	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close with running load = %v, want deadline exceeded", err)
	}
	if _, err := s.Load(context.Background(), Key{Kind: KindBrand, Host: "other.de"}, okFetch(&calls, nil), LoadOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Load after Close = %v, want ErrClosed", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- s.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(gate)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("in-flight load must still succeed: %v", err)
	}
}

func TestRedisDownIsStoreUnavailable(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	e.mr.Close()
	var calls atomic.Int32
	_, err := s.Load(context.Background(), testKey, okFetch(&calls, nil), LoadOptions{})
	if !errors.Is(err, ErrStoreUnavailable) || calls.Load() != 0 {
		t.Fatalf("err = %v calls=%d", err, calls.Load())
	}
	if err := s.Ping(context.Background()); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("ping = %v", err)
	}
}

func TestFetchPanicBecomesError(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	_, err := s.Load(context.Background(), testKey, func(context.Context) (Fetched, error) { panic("boom") }, LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v", err)
	}
	if e.mr.Exists(testKey.Lock()) {
		t.Fatal("lock leaked after panic")
	}
}

func TestGetOrLoadTyped(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	e := newEnv(t)
	s := e.store()
	var calls atomic.Int32
	fetch := func(context.Context) (payload, error) { calls.Add(1); return payload{Name: "ACME GmbH"}, nil }

	r1, err := GetOrLoad(context.Background(), s, testKey, LoadOptions{}, fetch)
	if err != nil || r1.Cached || r1.Data.Name != "ACME GmbH" || r1.Status != StatusOK {
		t.Fatalf("%+v %v", r1, err)
	}
	r2, err := GetOrLoad(context.Background(), s, testKey, LoadOptions{}, fetch)
	if err != nil || !r2.Cached || r2.Data.Name != "ACME GmbH" || calls.Load() != 1 {
		t.Fatalf("%+v %v calls=%d", r2, err, calls.Load())
	}

	// A payload of the wrong shape under the same key surfaces as a decode error.
	e.mr.Set(testKey.Cache(), `{"schema_version":1,"status":"ok","data":[1,2]}`)
	if _, err := GetOrLoad(context.Background(), s, testKey, LoadOptions{}, fetch); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestLoadAdmissionConcurrencyLimit(t *testing.T) {
	e := newEnv(t)
	s := e.store(WithLoadLimits(2, 0, 0))
	var calls atomic.Int32
	gate := make(chan struct{})
	done := make(chan error, 2)
	for _, host := range []string{"a.de", "b.de"} {
		go func() {
			_, err := s.Load(context.Background(), Key{Kind: KindImprint, Host: host}, okFetch(&calls, gate), LoadOptions{})
			done <- err
		}()
	}
	waitFor(t, func() bool { return calls.Load() == 2 })

	// A third distinct host is shed immediately, without touching upstream.
	_, err := s.Load(context.Background(), Key{Kind: KindImprint, Host: "c.de"}, okFetch(&calls, nil), LoadOptions{})
	if !errors.Is(err, ErrOverloaded) || calls.Load() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	if e.rec.get("rejected:load_concurrency") != 1 {
		t.Error("rejection metric missing")
	}
	if e.mr.Exists(Key{Kind: KindImprint, Host: "c.de"}.Cache()) {
		t.Error("an overload rejection must not be cached")
	}
	close(gate)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// The slot is free again.
	if _, err := s.Load(context.Background(), Key{Kind: KindImprint, Host: "c.de"}, okFetch(&calls, nil), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAdmissionRateLimit(t *testing.T) {
	e := newEnv(t)
	s := e.store(WithLoadLimits(0, 0.001, 2)) // burst of 2, then effectively nothing
	var calls atomic.Int32
	var rejected int
	for i := range 5 {
		_, err := s.Load(context.Background(), Key{Kind: KindBrand, Host: fmt.Sprintf("h%d.de", i)}, okFetch(&calls, nil), LoadOptions{})
		if errors.Is(err, ErrOverloaded) {
			rejected++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 || rejected != 3 {
		t.Fatalf("calls=%d rejected=%d, want 2 and 3", calls.Load(), rejected)
	}
	// Cache hits never pass through admission control.
	if entry, err := s.Load(context.Background(), Key{Kind: KindBrand, Host: "h0.de"}, okFetch(&calls, nil), LoadOptions{}); err != nil || !entry.Cached {
		t.Fatalf("hit under rate limit: %+v %v", entry, err)
	}
}

func TestInspectAndFlush(t *testing.T) {
	e := newEnv(t)
	s := e.store(WithRefreshCooldown(time.Hour))
	ctx := context.Background()

	info, err := s.Inspect(ctx, testKey)
	if err != nil || info.Exists || info.TTLSeconds != -1 || info.LockHeld || info.CooldownTTLSeconds != -1 {
		t.Fatalf("empty: %+v %v", info, err)
	}
	if info.CacheKey != testKey.Cache() || info.LockKey != testKey.Lock() {
		t.Fatalf("keys: %+v", info)
	}

	var calls atomic.Int32
	if _, err := s.Load(ctx, testKey, okFetch(&calls, nil), LoadOptions{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	info, err = s.Inspect(ctx, testKey)
	if err != nil || !info.Exists || info.Status != StatusOK || info.SizeBytes == 0 || info.AgeSeconds < 0 ||
		info.TTLSeconds < 29*24*3600 || info.CooldownTTLSeconds <= 0 || info.LockHeld {
		t.Fatalf("after load: %+v %v", info, err)
	}

	// The lock is reported while someone holds it.
	e.mr.Set(testKey.Lock(), "x")
	if info, _ = s.Inspect(ctx, testKey); !info.LockHeld {
		t.Fatal("lock must be reported")
	}
	e.mr.Del(testKey.Lock())

	n, err := s.Flush(ctx, testKey)
	if err != nil || n != 2 { // entry + cooldown
		t.Fatalf("flush = %d, %v", n, err)
	}
	if info, _ = s.Inspect(ctx, testKey); info.Exists || info.CooldownTTLSeconds != -1 {
		t.Fatalf("after flush: %+v", info)
	}
	if _, err := s.Load(ctx, testKey, okFetch(&calls, nil), LoadOptions{}); err != nil || calls.Load() != 2 {
		t.Fatalf("a flushed entry must be reloaded: err=%v calls=%d", err, calls.Load())
	}
}

func TestInspectRedisDown(t *testing.T) {
	e := newEnv(t)
	s := e.store()
	e.mr.Close()
	if _, err := s.Inspect(context.Background(), testKey); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Flush(context.Background(), testKey); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

type partialPayload struct {
	Name    string `json:"name"`
	Degrade bool   `json:"degrade"`
}

func (p partialPayload) PartialResult() bool { return p.Degrade }

func TestPartialResultsGetTheShortTTL(t *testing.T) {
	e := newEnv(t)
	s := e.store(WithTTLs(TTLs{ImprintOK: 30 * 24 * time.Hour, BrandOK: time.Hour, NotFound: time.Hour, Error: time.Minute, Partial: 2 * time.Hour}))
	ctx := context.Background()
	full, degraded := Key{Kind: KindImprint, Host: "full.de"}, Key{Kind: KindImprint, Host: "degraded.de"}

	if _, err := GetOrLoad(ctx, s, full, LoadOptions{}, func(context.Context) (partialPayload, error) { return partialPayload{Name: "a"}, nil }); err != nil {
		t.Fatal(err)
	}
	r, err := GetOrLoad(ctx, s, degraded, LoadOptions{}, func(context.Context) (partialPayload, error) {
		return partialPayload{Name: "b", Degrade: true}, nil
	})
	if err != nil || r.Status != StatusOK || r.Data.Name != "b" {
		t.Fatalf("a partial result is still served: %+v %v", r, err)
	}
	if ttl := e.mr.TTL(full.Cache()); ttl != 30*24*time.Hour {
		t.Errorf("full ttl = %s", ttl)
	}
	if ttl := e.mr.TTL(degraded.Cache()); ttl != 2*time.Hour {
		t.Errorf("partial ttl = %s, want 2h", ttl)
	}
	if info, _ := s.Inspect(ctx, degraded); !info.Partial {
		t.Error("Inspect must report the partial flag")
	}
}
