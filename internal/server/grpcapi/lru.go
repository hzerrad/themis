package grpcapi

import (
	"container/list"
	"sync"
)

// lruSize bounds each lru, so callers can't grow it without limit.
const lruSize = 4096

// lru keeps the most recently used values, up to lruSize. Its zero value is
// ready to use, and it is safe for concurrent use.
type lru[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]*list.Element
	order list.List // most recent first
}

type lruEntry[K comparable, V any] struct {
	key   K
	value V
}

func (c *lru[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.lookup(k); ok {
		return e.value, true
	}
	var zero V
	return zero, false
}

func (c *lru[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.lookup(k); ok {
		e.value = v
		return
	}
	c.insert(k, v)
}

// getOrPut returns the value for k, storing a new one first if there is none.
func (c *lru[K, V]) getOrPut(k K, v func() V) V {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.lookup(k); ok {
		return e.value
	}
	nv := v()
	c.insert(k, nv)
	return nv
}

func (c *lru[K, V]) lookup(k K) (*lruEntry[K, V], bool) {
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry[K, V]), true //nolint:forcetypeassert // only entries are stored
}

func (c *lru[K, V]) insert(k K, v V) {
	if c.items == nil {
		c.items = map[K]*list.Element{}
	}
	c.items[k] = c.order.PushFront(&lruEntry[K, V]{k, v})
	if c.order.Len() > lruSize {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*lruEntry[K, V]).key) //nolint:forcetypeassert // only entries are stored
	}
}
