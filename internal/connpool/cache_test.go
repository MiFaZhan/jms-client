package connpool

import (
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/assets"
)

// TestAssetCacheHitsBeforeTTLAndMissesAtTTL pins the freshness rule against
// injected times only: no sleeping, and the boundary is exactly ResolveTTL.
func TestAssetCacheHitsBeforeTTLAndMissesAtTTL(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := NewAssetCache()
	info := assets.Info{ID: "uuid-1", Name: "web-01", Account: "@USER", Protocol: "ssh"}
	c.Put("bastion|web-01", info, base)

	if got, ok := c.Get("bastion|web-01", base); !ok || got != info {
		t.Fatalf("Get() right after Put = (%+v, %v), want the stored entry", got, ok)
	}
	if got, ok := c.Get("bastion|web-01", base.Add(ResolveTTL-time.Nanosecond)); !ok || got != info {
		t.Fatalf("Get() just inside the TTL = (%+v, %v), want a hit", got, ok)
	}
	if _, ok := c.Get("bastion|web-01", base.Add(ResolveTTL)); ok {
		t.Fatal("Get() at exactly the TTL = hit, want a miss")
	}
	if _, ok := c.Get("bastion|web-01", base.Add(ResolveTTL+time.Minute)); ok {
		t.Fatal("Get() past the TTL = hit, want a miss")
	}
}

// TestAssetCacheIsKeyedPerServer proves the key includes the server alias: the
// same asset name on two JumpServers is two different resolutions
// (DESIGN.md §4.4).
func TestAssetCacheIsKeyedPerServer(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := NewAssetCache()
	c.Put("bastion|web-01", assets.Info{ID: "uuid-a"}, now)
	c.Put("prod|web-01", assets.Info{ID: "uuid-b"}, now)

	a, okA := c.Get("bastion|web-01", now)
	b, okB := c.Get("prod|web-01", now)
	if !okA || !okB {
		t.Fatalf("both keys must be cached, got (%v, %v)", okA, okB)
	}
	if a.ID == b.ID {
		t.Fatalf("both servers returned asset %q; the key is not server-scoped", a.ID)
	}
}

// TestAssetCacheUnknownKeyMisses keeps a miss from being reported as a zero
// resolution.
func TestAssetCacheUnknownKeyMisses(t *testing.T) {
	c := NewAssetCache()
	if got, ok := c.Get("nobody|nowhere", time.Now()); ok {
		t.Fatalf("Get() for an unknown key = (%+v, true), want a miss", got)
	}
}

// TestAssetCacheInvalidateForcesMiss is the "config add/remove/set-default
// takes effect immediately" half of DESIGN.md §4.4.
func TestAssetCacheInvalidateForcesMiss(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := NewAssetCache()
	c.Put("bastion|web-01", assets.Info{ID: "uuid-1"}, now)

	if _, ok := c.Get("bastion|web-01", now); !ok {
		t.Fatal("Get() before Invalidate = miss, want a hit")
	}
	c.Invalidate("bastion|web-01")
	if _, ok := c.Get("bastion|web-01", now); ok {
		t.Fatal("Get() after Invalidate = hit, want a miss")
	}
	// Invalidate of an unknown key must be a no-op.
	c.Invalidate("bastion|web-01")
}

// TestAssetCachePutRefreshesTimestamp keeps a re-resolve from being treated as
// stale because the first Put was long ago.
func TestAssetCachePutRefreshesTimestamp(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := NewAssetCache()
	c.Put("bastion|web-01", assets.Info{ID: "uuid-old"}, base)

	later := base.Add(ResolveTTL + time.Minute)
	c.Put("bastion|web-01", assets.Info{ID: "uuid-new"}, later)

	got, ok := c.Get("bastion|web-01", later.Add(time.Second))
	if !ok || got.ID != "uuid-new" {
		t.Fatalf("Get() = (%+v, %v), want the refreshed entry", got, ok)
	}
}

// TestAssetCacheNilReceiverIsSafe covers the nil-receiver behaviour the frozen
// signatures allow.
func TestAssetCacheNilReceiverIsSafe(t *testing.T) {
	var c *AssetCache
	if _, ok := c.Get("bastion|web-01", time.Now()); ok {
		t.Fatal("nil cache Get() = hit, want a miss")
	}
	c.Put("bastion|web-01", assets.Info{ID: "uuid-1"}, time.Now())
	c.Invalidate("bastion|web-01")
}

// TestAssetCacheZeroValueIsUsable keeps the struct usable without the
// constructor, which is how a test or a caller may embed it.
func TestAssetCacheZeroValueIsUsable(t *testing.T) {
	var c AssetCache
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c.Put("bastion|web-01", assets.Info{ID: "uuid-1"}, now)
	if got, ok := c.Get("bastion|web-01", now); !ok || got.ID != "uuid-1" {
		t.Fatalf("Get() = (%+v, %v), want the stored entry", got, ok)
	}
}
