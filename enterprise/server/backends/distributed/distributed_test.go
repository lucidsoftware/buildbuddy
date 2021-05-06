package distributed

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/buildbuddy-io/buildbuddy/server/backends/memory_cache"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/app"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testauth"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testdigest"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_client"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/prefix"
	"github.com/buildbuddy-io/buildbuddy/server/util/testing/flags"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
)

var (
	emptyUserMap = testauth.TestUsers()

	// The maximum duration to wait for a server to become ready.
	maxReadyWaitTime = 3 * time.Second
)

func getTestEnv(t *testing.T, users map[string]interfaces.UserInfo) *testenv.TestEnv {
	te := testenv.GetTestEnv(t)
	te.SetAuthenticator(testauth.NewTestAuthenticator(users))
	return te
}

func getAnonContext(t *testing.T) context.Context {
	flags.Set(t, "auth.enable_anonymous_usage", "true")
	te := getTestEnv(t, emptyUserMap)
	ctx, err := prefix.AttachUserPrefixToContext(context.Background(), te)
	if err != nil {
		t.Errorf("error attaching user prefix: %v", err)
	}
	return ctx
}

func newMemoryCache(t *testing.T, maxSizeBytes int64) interfaces.Cache {
	mc, err := memory_cache.NewMemoryCache(maxSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

func waitForReady(t *testing.T, addr string) {
	log.Warningf("Waiting for peer %q to become ready!", addr)
	conn, err := grpc_client.DialTargetWithOptions("grpc://"+addr, false, grpc.WithBlock(), grpc.WithTimeout(maxReadyWaitTime))
	if err != nil {
		t.Fatal(err)
	}
	log.Warningf("Peer %q became ready!", addr)
	conn.Close()
}

func startNewDCache(t *testing.T, te environment.Env, config CacheConfig, baseCache interfaces.Cache) *Cache {
	c, err := NewDistributedCache(te, baseCache, config, te.GetHealthChecker())
	if err != nil {
		t.Fatal(err)
	}
	c.StartListening()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		c.Shutdown(shutdownCtx)
		cancel()
	})
	return c
}

func readAndCompareDigest(t *testing.T, ctx context.Context, c interfaces.Cache, d *repb.Digest) {
	reader, err := c.Reader(ctx, d, 0)
	if err != nil {
		assert.FailNow(t, fmt.Sprintf("cache: %+v", c), err)
	}
	d1 := testdigest.ReadDigestAndClose(t, reader)
	assert.Equal(t, d.GetHash(), d1.GetHash())
}

func TestBasicReadWrite(t *testing.T) {
	te := getTestEnv(t, emptyUserMap)
	ctx := getAnonContext(t)
	singleCacheSizeBytes := int64(1000000)
	peer1 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer2 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer3 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	baseConfig := CacheConfig{
		ReplicationFactor:  3,
		Nodes:              []string{peer1, peer2, peer3},
		DisableLocalLookup: true,
	}

	// Setup a distributed cache, 3 nodes, R = 3.
	memoryCache1 := newMemoryCache(t, singleCacheSizeBytes)
	config1 := baseConfig
	config1.ListenAddr = peer1
	dc1 := startNewDCache(t, te, config1, memoryCache1)

	memoryCache2 := newMemoryCache(t, singleCacheSizeBytes)
	config2 := baseConfig
	config2.ListenAddr = peer2
	dc2 := startNewDCache(t, te, config2, memoryCache2)

	memoryCache3 := newMemoryCache(t, singleCacheSizeBytes)
	config3 := baseConfig
	config3.ListenAddr = peer3
	dc3 := startNewDCache(t, te, config3, memoryCache3)

	waitForReady(t, config1.ListenAddr)
	waitForReady(t, config2.ListenAddr)
	waitForReady(t, config3.ListenAddr)

	baseCaches := []interfaces.Cache{
		memoryCache1,
		memoryCache2,
		memoryCache3,
	}
	distributedCaches := []interfaces.Cache{dc1, dc2, dc3}

	for i := 0; i < 100; i++ {
		// Do a write, and ensure it was written to all nodes.
		d, buf := testdigest.NewRandomDigestBuf(t, 100)
		if err := distributedCaches[i%3].Set(ctx, d, buf); err != nil {
			t.Fatal(err)
		}
		for _, baseCache := range baseCaches {
			exists, err := baseCache.Contains(ctx, d)
			assert.Nil(t, err)
			assert.True(t, exists)
			readAndCompareDigest(t, ctx, baseCache, d)
		}
	}
}

func TestReadWriteWithFailedNode(t *testing.T) {
	te := getTestEnv(t, emptyUserMap)
	ctx := getAnonContext(t)
	singleCacheSizeBytes := int64(1000000)
	peer1 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer2 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer3 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer4 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	baseConfig := CacheConfig{
		ReplicationFactor:  3,
		Nodes:              []string{peer1, peer2, peer3, peer4},
		DisableLocalLookup: true,
	}

	// Setup a distributed cache, 4 nodes, R = 3.
	memoryCache1 := newMemoryCache(t, singleCacheSizeBytes)
	config1 := baseConfig
	config1.ListenAddr = peer1
	dc1 := startNewDCache(t, te, config1, memoryCache1)

	memoryCache2 := newMemoryCache(t, singleCacheSizeBytes)
	config2 := baseConfig
	config2.ListenAddr = peer2
	dc2 := startNewDCache(t, te, config2, memoryCache2)

	memoryCache3 := newMemoryCache(t, singleCacheSizeBytes)
	config3 := baseConfig
	config3.ListenAddr = peer3
	dc3 := startNewDCache(t, te, config3, memoryCache3)

	memoryCache4 := newMemoryCache(t, singleCacheSizeBytes)
	config4 := baseConfig
	config4.ListenAddr = peer4
	dc4 := startNewDCache(t, te, config4, memoryCache4)

	waitForReady(t, config1.ListenAddr)
	waitForReady(t, config2.ListenAddr)
	waitForReady(t, config3.ListenAddr)
	waitForReady(t, config4.ListenAddr)

	baseCaches := []interfaces.Cache{memoryCache1, memoryCache2, memoryCache4}
	distributedCaches := []interfaces.Cache{dc1, dc2, dc4}

	// "Fail" a a node by shutting it down.
	// The basecache and distributed cache are not in baseCaches
	// or distributedCaches so they should not be referenced
	// below when reading / writing, although the running nodes
	// still have reference to them via the Nodes list.
	shutdownCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	err := dc3.Shutdown(shutdownCtx)
	cancel()
	assert.Nil(t, err)

	for i := 0; i < 100; i++ {
		// Do a write, and ensure it was written to all nodes.
		d, buf := testdigest.NewRandomDigestBuf(t, 100)
		j := i % len(distributedCaches)
		if err := distributedCaches[j].Set(ctx, d, buf); err != nil {
			t.Fatal(err)
		}
		for _, baseCache := range baseCaches {
			exists, err := baseCache.Contains(ctx, d)
			assert.Nil(t, err)
			assert.True(t, exists)
			readAndCompareDigest(t, ctx, baseCache, d)
		}
	}
}

func TestReadWriteWithFailedAndRestoredNode(t *testing.T) {
	te := getTestEnv(t, emptyUserMap)
	ctx := getAnonContext(t)
	singleCacheSizeBytes := int64(1000000)
	peer1 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer2 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer3 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer4 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	baseConfig := CacheConfig{
		ReplicationFactor:    3,
		Nodes:                []string{peer1, peer2, peer3, peer4},
		DisableLocalLookup:   true,
		RPCHeartbeatInterval: 100 * time.Millisecond,
	}

	// Setup a distributed cache, 4 nodes, R = 3.
	memoryCache1 := newMemoryCache(t, singleCacheSizeBytes)
	config1 := baseConfig
	config1.ListenAddr = peer1
	dc1 := startNewDCache(t, te, config1, memoryCache1)

	memoryCache2 := newMemoryCache(t, singleCacheSizeBytes)
	config2 := baseConfig
	config2.ListenAddr = peer2
	dc2 := startNewDCache(t, te, config2, memoryCache2)

	memoryCache3 := newMemoryCache(t, singleCacheSizeBytes)
	config3 := baseConfig
	config3.ListenAddr = peer3
	dc3 := startNewDCache(t, te, config3, memoryCache3)

	memoryCache4 := newMemoryCache(t, singleCacheSizeBytes)
	config4 := baseConfig
	config4.ListenAddr = peer4
	dc4 := startNewDCache(t, te, config4, memoryCache4)

	waitForReady(t, config1.ListenAddr)
	waitForReady(t, config2.ListenAddr)
	waitForReady(t, config3.ListenAddr)
	waitForReady(t, config4.ListenAddr)

	baseCaches := []interfaces.Cache{memoryCache1, memoryCache2, memoryCache4}
	distributedCaches := []interfaces.Cache{dc1, dc2, dc4}

	// "Fail" a a node by shutting it down.
	// The basecache and distributed cache are not in baseCaches
	// or distributedCaches so they should not be referenced
	// below when reading / writing, although the running nodes
	// still have reference to them via the Nodes list.
	shutdownCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	err := dc3.Shutdown(shutdownCtx)
	cancel()
	assert.Nil(t, err)

	digestsWritten := make([]*repb.Digest, 0)
	for i := 0; i < 100; i++ {
		// Do a write, and ensure it was written to all nodes.
		d, buf := testdigest.NewRandomDigestBuf(t, 100)
		j := i % len(distributedCaches)
		if err := distributedCaches[j].Set(ctx, d, buf); err != nil {
			t.Fatal(err)
		}
		digestsWritten = append(digestsWritten, d)
		for _, baseCache := range baseCaches {
			exists, err := baseCache.Contains(ctx, d)
			assert.Nil(t, err)
			assert.True(t, exists)
			readAndCompareDigest(t, ctx, baseCache, d)
		}
	}

	baseCaches = append(baseCaches, memoryCache3)
	distributedCaches = append(distributedCaches, dc3)
	dc3.StartListening()
	waitForReady(t, config3.ListenAddr)
	for _, d := range digestsWritten {
		for _, distributedCache := range distributedCaches {
			exists, err := distributedCache.Contains(ctx, d)
			assert.Nil(t, err)
			assert.True(t, exists)
			readAndCompareDigest(t, ctx, distributedCache, d)
		}
	}
}

func TestBackfill(t *testing.T) {
	te := getTestEnv(t, emptyUserMap)
	ctx := getAnonContext(t)
	singleCacheSizeBytes := int64(1000000)
	peer1 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer2 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer3 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	baseConfig := CacheConfig{
		ReplicationFactor:    3,
		Nodes:                []string{peer1, peer2, peer3},
		DisableLocalLookup:   true,
		RPCHeartbeatInterval: 100 * time.Millisecond,
	}

	// Setup a distributed cache, 3 nodes, R = 3.
	memoryCache1 := newMemoryCache(t, singleCacheSizeBytes)
	config1 := baseConfig
	config1.ListenAddr = peer1
	dc1 := startNewDCache(t, te, config1, memoryCache1)

	memoryCache2 := newMemoryCache(t, singleCacheSizeBytes)
	config2 := baseConfig
	config2.ListenAddr = peer2
	dc2 := startNewDCache(t, te, config2, memoryCache2)

	memoryCache3 := newMemoryCache(t, singleCacheSizeBytes)
	config3 := baseConfig
	config3.ListenAddr = peer3
	dc3 := startNewDCache(t, te, config3, memoryCache3)

	waitForReady(t, config1.ListenAddr)
	waitForReady(t, config2.ListenAddr)
	waitForReady(t, config3.ListenAddr)

	baseCaches := []interfaces.Cache{memoryCache1, memoryCache2, memoryCache3}
	distributedCaches := []interfaces.Cache{dc1, dc2, dc3}

	digestsWritten := make([]*repb.Digest, 0)
	for i := 0; i < 100; i++ {
		// Do a write, and ensure it was written to all nodes.
		d, buf := testdigest.NewRandomDigestBuf(t, 100)
		j := i % len(distributedCaches)
		if err := distributedCaches[j].Set(ctx, d, buf); err != nil {
			t.Fatal(err)
		}
		digestsWritten = append(digestsWritten, d)
		for _, baseCache := range baseCaches {
			exists, err := baseCache.Contains(ctx, d)
			assert.Nil(t, err)
			assert.True(t, exists)
			readAndCompareDigest(t, ctx, baseCache, d)
		}
	}

	// Now zero out one of the base caches.
	for _, d := range digestsWritten {
		if err := memoryCache3.Delete(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	// Read our digests, and ensure that after each read, the digest
	// is *also* present in the base cache of the zeroed-out node,
	// because it has been backfilled.
	for _, d := range digestsWritten {
		for _, distributedCache := range distributedCaches {
			exists, err := distributedCache.Contains(ctx, d)
			assert.Nil(t, err)
			assert.True(t, exists)
			readAndCompareDigest(t, ctx, distributedCache, d)
		}
		for i, baseCache := range baseCaches {
			exists, err := baseCache.Contains(ctx, d)
			assert.Nil(t, err, fmt.Sprintf("basecache %dmissing digest", i))
			assert.True(t, exists, fmt.Sprintf("basecache %dmissing digest", i))
			readAndCompareDigest(t, ctx, baseCache, d)
		}
	}
}

func TestContainsMulti(t *testing.T) {
	te := getTestEnv(t, emptyUserMap)
	ctx := getAnonContext(t)
	singleCacheSizeBytes := int64(1000000)
	peer1 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer2 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer3 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	baseConfig := CacheConfig{
		ReplicationFactor:  3,
		Nodes:              []string{peer1, peer2, peer3},
		DisableLocalLookup: true,
	}

	// Setup a distributed cache, 3 nodes, R = 3.
	memoryCache1 := newMemoryCache(t, singleCacheSizeBytes)
	config1 := baseConfig
	config1.ListenAddr = peer1
	dc1 := startNewDCache(t, te, config1, memoryCache1)

	memoryCache2 := newMemoryCache(t, singleCacheSizeBytes)
	config2 := baseConfig
	config2.ListenAddr = peer2
	dc2 := startNewDCache(t, te, config2, memoryCache2)

	memoryCache3 := newMemoryCache(t, singleCacheSizeBytes)
	config3 := baseConfig
	config3.ListenAddr = peer3
	dc3 := startNewDCache(t, te, config3, memoryCache3)

	waitForReady(t, config1.ListenAddr)
	waitForReady(t, config2.ListenAddr)
	waitForReady(t, config3.ListenAddr)

	baseCaches := []interfaces.Cache{
		memoryCache1,
		memoryCache2,
		memoryCache3,
	}
	distributedCaches := []interfaces.Cache{dc1, dc2, dc3}

	digestsWritten := make([]*repb.Digest, 0)
	for i := 0; i < 100; i++ {
		// Do a write, and ensure it was written to all nodes.
		d, buf := testdigest.NewRandomDigestBuf(t, 100)
		if err := distributedCaches[i%3].Set(ctx, d, buf); err != nil {
			t.Fatal(err)
		}
		digestsWritten = append(digestsWritten, d)
	}

	for _, baseCache := range baseCaches {
		foundMap, err := baseCache.ContainsMulti(ctx, digestsWritten)
		assert.Nil(t, err)
		for _, d := range digestsWritten {
			exists, ok := foundMap[d]
			assert.True(t, ok)
			assert.True(t, exists)
		}
	}

	for _, distributedCache := range distributedCaches {
		foundMap, err := distributedCache.ContainsMulti(ctx, digestsWritten)
		assert.Nil(t, err)
		for _, d := range digestsWritten {
			exists, ok := foundMap[d]
			assert.True(t, ok)
			assert.True(t, exists)
		}
	}
}

func TestGetMulti(t *testing.T) {
	t.Skip()
	te := getTestEnv(t, emptyUserMap)
	ctx := getAnonContext(t)
	singleCacheSizeBytes := int64(1000000)
	peer1 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer2 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	peer3 := fmt.Sprintf("localhost:%d", app.FreePort(t))
	baseConfig := CacheConfig{
		ReplicationFactor:  3,
		Nodes:              []string{peer1, peer2, peer3},
		DisableLocalLookup: true,
	}

	// Setup a distributed cache, 3 nodes, R = 3.
	memoryCache1 := newMemoryCache(t, singleCacheSizeBytes)
	config1 := baseConfig
	config1.ListenAddr = peer1
	dc1 := startNewDCache(t, te, config1, memoryCache1)

	memoryCache2 := newMemoryCache(t, singleCacheSizeBytes)
	config2 := baseConfig
	config2.ListenAddr = peer2
	dc2 := startNewDCache(t, te, config2, memoryCache2)

	memoryCache3 := newMemoryCache(t, singleCacheSizeBytes)
	config3 := baseConfig
	config3.ListenAddr = peer3
	dc3 := startNewDCache(t, te, config3, memoryCache3)

	waitForReady(t, config1.ListenAddr)
	waitForReady(t, config2.ListenAddr)
	waitForReady(t, config3.ListenAddr)

	baseCaches := []interfaces.Cache{
		memoryCache1,
		memoryCache2,
		memoryCache3,
	}
	distributedCaches := []interfaces.Cache{dc1, dc2, dc3}

	digestsWritten := make([]*repb.Digest, 0)
	for i := 0; i < 100; i++ {
		// Do a write, and ensure it was written to all nodes.
		d, buf := testdigest.NewRandomDigestBuf(t, 100)
		if err := distributedCaches[i%3].Set(ctx, d, buf); err != nil {
			t.Fatal(err)
		}
		digestsWritten = append(digestsWritten, d)
	}

	for _, baseCache := range baseCaches {
		gotMap, err := baseCache.GetMulti(ctx, digestsWritten)
		assert.Nil(t, err)
		for _, d := range digestsWritten {
			buf, ok := gotMap[d]
			assert.True(t, ok)
			assert.Equal(t, d.GetSizeBytes(), int64(len(buf)))
		}
	}

	for _, distributedCache := range distributedCaches {
		gotMap, err := distributedCache.GetMulti(ctx, digestsWritten)
		assert.Nil(t, err)
		for _, d := range digestsWritten {
			buf, ok := gotMap[d]
			assert.True(t, ok)
			assert.Equal(t, d.GetSizeBytes(), int64(len(buf)))
		}
	}
}
