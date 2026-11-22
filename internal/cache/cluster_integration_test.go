//go:build integration

package cache

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Run against a real Redis Cluster (miniredis cannot do cluster mode):
//
//	SITEINFO_TEST_REDIS_CLUSTER=127.0.0.1:7000 go test -tags integration -run Cluster ./internal/cache/
func TestClusterReadThroughAndHashTags(t *testing.T) {
	addrs := os.Getenv("SITEINFO_TEST_REDIS_CLUSTER")
	if addrs == "" {
		t.Skip("SITEINFO_TEST_REDIS_CLUSTER not set")
	}
	rdb := redis.NewClusterClient(&redis.ClusterOptions{Addrs: strings.Split(addrs, ",")})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	// Cache, lock and cooldown keys of a host must share one slot.
	for _, host := range []string{"example.de", "xn--bcher-kva.de", "a.b.example.ch"} {
		k := Key{Kind: KindImprint, Host: host}
		slot := func(key string) int64 { return rdb.ClusterKeySlot(ctx, key).Val() }
		if slot(k.Cache()) != slot(k.Lock()) || slot(k.Cache()) != slot(k.Cooldown()) {
			t.Errorf("%s: keys are in different slots", host)
		}
	}

	s := NewStore(rdb, WithLoadTimeout(10*time.Second), WithLockMargin(5*time.Second))
	run := time.Now().UnixNano()
	var wg sync.WaitGroup
	var calls [3]atomic.Int32
	for h := range 3 { // three hosts, probably on different masters
		host := "cluster-test-" + string(rune('a'+h)) + "-" + time.Unix(0, run).Format("150405.000000") + ".de"
		t.Cleanup(func() { rdb.Del(ctx, Key{KindImprint, host}.Cache(), Key{KindImprint, host}.Cooldown()) })
		for range 10 {
			wg.Go(func() {
				_, err := s.Load(ctx, Key{Kind: KindImprint, Host: host}, func(context.Context) (json.RawMessage, error) {
					calls[h].Add(1)
					time.Sleep(200 * time.Millisecond)
					return json.RawMessage(`{"ok":true}`), nil
				}, LoadOptions{})
				if err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	for h := range calls {
		if n := calls[h].Load(); n != 1 {
			t.Errorf("host %d: upstream calls = %d, want 1", h, n)
		}
	}
}
