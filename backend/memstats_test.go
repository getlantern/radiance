package backend

import (
	"path/filepath"
	"testing"

	"github.com/sagernet/sing/common/buf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubAllocator records delegation so tests can assert when bufAlloc falls through
// to inner (limit == 0) vs. uses its own bounded free-lists (limit > 0).
type stubAllocator struct {
	gets, puts int
}

func (s *stubAllocator) Get(size int) []byte { s.gets++; return make([]byte, size) }
func (s *stubAllocator) Put([]byte) error    { s.puts++; return nil }

func TestBufClass(t *testing.T) {
	cases := []struct {
		size, want int
	}{
		{1, 0}, {64, 0}, {65, 1}, {128, 1}, {129, 2},
		{4096, 6}, {8192, 7}, {8193, 8}, {16384, 8}, {32768, 9}, {65536, 10},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, bufClass(c.size), "bufClass(%d)", c.size)
	}
}

func TestBufAllocUnbounded(t *testing.T) {
	stub := &stubAllocator{}
	a := &bufAlloc{inner: stub}
	a.SetLimit(0)

	b := a.Get(8192)
	require.NoError(t, a.Put(b))

	assert.Equal(t, 1, stub.gets, "inner.Get calls")
	assert.Equal(t, 1, stub.puts, "inner.Put calls")
	assert.Equal(t, int64(1), a.gets.Load(), "gets")
	assert.Equal(t, int64(1), a.puts.Load(), "puts")
	assert.Equal(t, int64(0), a.live.Load(), "live")
}

func TestBufAllocBoundedRetention(t *testing.T) {
	stub := &stubAllocator{}
	a := &bufAlloc{inner: stub}
	a.SetLimit(2)

	// Put four 8 KB buffers; only the cap (2) should be retained, the rest dropped.
	for i := 0; i < 4; i++ {
		require.NoError(t, a.Put(make([]byte, 8192)))
	}
	assert.Len(t, a.pools[bufClass(8192)], 2, "retained (cap)")
	assert.Zero(t, stub.puts, "inner.Put must not be used in bounded mode")

	// First two Gets reuse pooled buffers; the third allocates fresh.
	first := a.Get(8192)
	assert.Equal(t, 8192, cap(first), "reused buffer cap")
	a.Get(8192)
	a.Get(8192)
	assert.Empty(t, a.pools[bufClass(8192)], "pool should be drained")
	assert.Zero(t, stub.gets, "inner.Get must not be used in bounded mode")
}

func TestBufAllocOversize(t *testing.T) {
	a := &bufAlloc{inner: &stubAllocator{}}
	a.SetLimit(2)
	assert.Nil(t, a.Get(maxBufSize+1))
}

// Guards against drift between bufAlloc's size classes and sing's allocator: a
// buffer from sing must round-trip through Put without rejection.
func TestBufAllocClassesMatchSing(t *testing.T) {
	a := &bufAlloc{inner: &stubAllocator{}}
	a.SetLimit(1)
	for _, size := range []int{buf.UDPBufferSize, buf.BufferSize} {
		assert.NoErrorf(t, a.Put(make([]byte, size)), "Put(cap=%d)", size)
	}
}

func TestRecommendedBufLimit(t *testing.T) {
	cases := []struct {
		peak int64
		want int
	}{
		{0, 256},     // floored
		{100, 256},   // 125 -> floored
		{645, 832},   // 806 -> round up to 832
		{1000, 1280}, // 1250 -> round up to 1280
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, recommendedBufLimit(c.peak), "recommendedBufLimit(%d)", c.peak)
	}
}

func TestBufPoolTuningRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), bufPoolTuningFile)
	_, ok := readBufPoolTuning(path)
	assert.False(t, ok, "read of missing file should report not-ok")

	want := bufPoolTuning{ObservedPeakLive: 645, RecommendedLimit: 832}
	require.NoError(t, writeBufPoolTuning(path, want))

	got, ok := readBufPoolTuning(path)
	require.True(t, ok)
	assert.Equal(t, want, got)
}
