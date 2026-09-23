package db

import "testing"

// The values before the write buffer was capped, kept here as the reference the
// cap must not deviate from on configurations that already existed.
func oldLevelDBCacheSizes(cache int) (handles, blockCacheMiB, writeBufferMiB int) {
	if cache == 0 {
		cache = 64
	}
	handles = cache
	if handles < 16 {
		handles = 16
	}
	if cache < 4 {
		cache = 4
	}
	blockCacheMiB = cache / 2
	writeBufferMiB = cache / 4
	return
}

func TestLevelDBCacheSizes(t *testing.T) {
	cases := []struct {
		cache                               int
		handles, blockCacheMiB, writeBufMiB int
	}{
		{0, 64, 32, 16}, // 0 falls back to the default 64
		{4, 16, 2, 1},   // smallest used in the shipped configs (addrbook)
		{16, 16, 8, 4},
		{64, 64, 32, 16},   // the default: cap not reached
		{128, 128, 64, 16}, // capped from here on
		{256, 256, 128, 16},
		{1024, 1024, 512, 16},
		{4096, 4096, 2048, 16},
	}
	for _, c := range cases {
		handles, blockCacheMiB, writeBufMiB := levelDBCacheSizes(c.cache, 0)
		if handles != c.handles || blockCacheMiB != c.blockCacheMiB || writeBufMiB != c.writeBufMiB {
			t.Errorf("levelDBCacheSizes(%d, 0) = (%d, %d, %d), want (%d, %d, %d)",
				c.cache, handles, blockCacheMiB, writeBufMiB, c.handles, c.blockCacheMiB, c.writeBufMiB)
		}
	}
}

// An explicit dbWriteBuffer overrides the derived value, whatever dbCache is.
// Only the write buffer changes; the read side is untouched.
func TestLevelDBCacheSizesExplicitWriteBuffer(t *testing.T) {
	for _, c := range []struct{ cache, writeBuffer int }{
		{64, 8}, {64, 32}, {256, 16}, {1024, 128}, {4, 64},
	} {
		handles, blockCacheMiB, writeBufMiB := levelDBCacheSizes(c.cache, c.writeBuffer)
		defHandles, defBlockCacheMiB, _ := levelDBCacheSizes(c.cache, 0)
		if writeBufMiB != c.writeBuffer {
			t.Errorf("levelDBCacheSizes(%d, %d): write buffer = %d, want the explicit %d",
				c.cache, c.writeBuffer, writeBufMiB, c.writeBuffer)
		}
		if handles != defHandles || blockCacheMiB != defBlockCacheMiB {
			t.Errorf("levelDBCacheSizes(%d, %d): read side = (%d, %d), want it unchanged at (%d, %d)",
				c.cache, c.writeBuffer, handles, blockCacheMiB, defHandles, defBlockCacheMiB)
		}
	}
}

// The cap must be a no-op for every configuration that could already exist: it
// may only kick in above the default dbCache, so no deployed node changes
// behaviour by upgrading.
func TestLevelDBCacheSizesUnchangedAtDefaults(t *testing.T) {
	for cache := 0; cache <= 64; cache++ {
		handles, blockCacheMiB, writeBufMiB := levelDBCacheSizes(cache, 0)
		oldHandles, oldBlockCacheMiB, oldWriteBufMiB := oldLevelDBCacheSizes(cache)
		if handles != oldHandles || blockCacheMiB != oldBlockCacheMiB || writeBufMiB != oldWriteBufMiB {
			t.Errorf("dbCache=%d changed behaviour: got (%d, %d, %d), before the cap it was (%d, %d, %d)",
				cache, handles, blockCacheMiB, writeBufMiB, oldHandles, oldBlockCacheMiB, oldWriteBufMiB)
		}
	}
}

// Above 2048 the cap must be the only thing that keeps the write buffer from
// growing without bound.
func TestLevelDBCacheSizesWriteBufferStaysCapped(t *testing.T) {
	for _, cache := range []int{128, 256, 512, 1024, 4096, 65536} {
		if _, _, writeBufMiB := levelDBCacheSizes(cache, 0); writeBufMiB != maxWriteBufferMiB {
			t.Errorf("dbCache=%d: write buffer = %d MiB, want it capped at %d MiB",
				cache, writeBufMiB, maxWriteBufferMiB)
		}
	}
}
