package artifact

import (
	"container/list"
	"sync"
)

// cache is a least-recently-used cache of trees bounded by their total bytes. Keys name
// content by digest (plus the selected path), so an entry never goes stale.
type cache struct {
	mu    sync.Mutex
	max   int64
	used  int64
	order *list.List // front: most recently used; values are *cacheItem
	items map[string]*list.Element
}

type cacheItem struct {
	key  string
	tree *Tree
}

func newCache(maxBytes int64) *cache {
	return &cache{max: maxBytes, order: list.New(), items: map[string]*list.Element{}}
}

func (c *cache) get(key string) (*Tree, bool) {
	if c.max <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheItem).tree, true
}

// put stores t unless it alone is larger than the cache, evicting the least recently used.
func (c *cache) put(key string, t *Tree) {
	if c.max <= 0 || t.Bytes > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.used -= el.Value.(*cacheItem).tree.Bytes
		c.order.Remove(el)
	}
	c.items[key] = c.order.PushFront(&cacheItem{key: key, tree: t})
	c.used += t.Bytes
	for c.used > c.max {
		el := c.order.Back()
		it := el.Value.(*cacheItem)
		c.order.Remove(el)
		delete(c.items, it.key)
		c.used -= it.tree.Bytes
	}
}

// size returns the number of entries and their bytes.
func (c *cache) size() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.used
}
