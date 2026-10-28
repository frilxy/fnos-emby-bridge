package emby

import (
	"strings"
	"sync"
	"time"
)

// ttlCache 是带"同 key 请求合并（singleflight）"的轻量缓存。
// 用于媒体库列表、详情、play/info、stream/list、tag 字典等重复度高的读请求。
// 注意：缓存值必须视为不可变；调用方拿到切片后要复制再排序/过滤。
type ttlCache struct {
	mu    sync.Mutex
	items map[string]cacheItem
	calls map[string]*cacheCall
}

type cacheItem struct {
	val     any
	expires time.Time
}

type cacheCall struct {
	done chan struct{}
	val  any
	err  error
}

func newTTLCache() *ttlCache {
	return &ttlCache{items: map[string]cacheItem{}, calls: map[string]*cacheCall{}}
}

// Do 命中未过期缓存直接返回；同 key 并发只执行一次 fn。
func (c *ttlCache) Do(key string, ttl time.Duration, fn func() (any, error)) (any, error) {
	c.mu.Lock()
	if it, ok := c.items[key]; ok && time.Now().Before(it.expires) {
		v := it.val
		c.mu.Unlock()
		return v, nil
	}
	if call, ok := c.calls[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.val, call.err
	}
	call := &cacheCall{done: make(chan struct{})}
	c.calls[key] = call
	c.mu.Unlock()

	v, err := fn()

	c.mu.Lock()
	if err == nil {
		c.items[key] = cacheItem{val: v, expires: time.Now().Add(ttl)}
	}
	call.val, call.err = v, err
	delete(c.calls, key)
	close(call.done)
	c.mu.Unlock()
	return v, err
}

// Delete 删除单个 key（写操作后失效用）。
func (c *ttlCache) Delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

// DeletePrefix 删除指定前缀的 key（写操作后清理某用户/某条目的列表缓存）。
func (c *ttlCache) DeletePrefix(prefix string) {
	c.mu.Lock()
	for k := range c.items {
		if strings.HasPrefix(k, prefix) {
			delete(c.items, k)
		}
	}
	c.mu.Unlock()
}

// Clear 清空全部缓存（库扫描/刷新后可用）。
func (c *ttlCache) Clear() {
	c.mu.Lock()
	c.items = map[string]cacheItem{}
	c.mu.Unlock()
}
