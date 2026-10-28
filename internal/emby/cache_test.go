package emby

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 同 key 并发只执行一次 fn（singleflight），之后的请求命中缓存。
func TestTTLCacheSingleflight(t *testing.T) {
	c := newTTLCache()
	var calls int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Do("k", time.Minute, func() (any, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(20 * time.Millisecond)
				return "v", nil
			})
			if err != nil || v != "v" {
				t.Errorf("Do = %v, %v", v, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("并发 20 次应只执行 1 次 fn，实际 %d", calls)
	}
	if _, _ = c.Do("k", time.Minute, func() (any, error) {
		atomic.AddInt32(&calls, 1)
		return "v", nil
	}); calls != 1 {
		t.Fatalf("缓存命中不应再执行 fn，实际 %d", calls)
	}
	c.Delete("k")
}
