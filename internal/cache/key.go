// Package cache implements the Redis read-through cache with an in-pod
// singleflight and a distributed redsync lock.
package cache

import "fmt"

// Kind identifies what is cached for a host.
type Kind string

// Cached kinds.
const (
	KindImprint Kind = "imprint"
	KindBrand   Kind = "brand"
)

// keyVersion is part of every Redis key so an incompatible schema change can
// roll out under a new key space instead of migrating data.
const keyVersion = "v2"

// Key addresses one cached entry. All keys of a host share the Redis Cluster
// hash tag "{host}", so cache, lock and cooldown keys land in the same slot.
type Key struct {
	Kind Kind
	Host string
}

// Cache is the Redis key holding the envelope, e.g. "imprint:v2:{example.de}".
func (k Key) Cache() string { return fmt.Sprintf("%s:%s:{%s}", k.Kind, keyVersion, k.Host) }

// Lock is the redsync mutex key.
func (k Key) Lock() string { return "lock:" + k.Cache() }

// Cooldown is the per-key refresh cooldown marker.
func (k Key) Cooldown() string { return "cooldown:" + k.Cache() }
