package router

import (
	"testing"
	"time"
)

func resetSnapshotCache() {
	lastSnapshotMu.Lock()
	defer lastSnapshotMu.Unlock()
	lastSnapshot = nil
	lastSnapshotAt = time.Time{}
}

func TestCachedSystemSnapshotDedupes(t *testing.T) {
	oldTTL := snapshotCacheTTL
	snapshotCacheTTL = time.Millisecond
	defer func() { snapshotCacheTTL = oldTTL }()

	resetSnapshotCache()

	first, err := cachedSystemSnapshot()
	if err != nil {
		t.Fatalf("first collection failed: %v", err)
	}

	second, err := cachedSystemSnapshot()
	if err != nil {
		t.Fatalf("second collection failed: %v", err)
	}
	if first != second {
		t.Fatal("expected the cached snapshot within the TTL window")
	}

	time.Sleep(2 * time.Millisecond)

	third, err := cachedSystemSnapshot()
	if err != nil {
		t.Fatalf("third collection failed: %v", err)
	}
	if first == third {
		t.Fatal("expected a fresh snapshot after the TTL window")
	}
}
