package indexes

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	BUCKET_COUNT = 16
	BUCKET_MASK  = BUCKET_COUNT - 1
)

var nullItem = &Item{
	expires: time.Now().Add(time.Hour * 1000),
	value:   nil,
}

type Fetcher interface {
	Fill([]interface{}, [][]byte) error
	Get(id Id) []byte
}

type Cache struct {
	gcing   uint32
	size    int64
	max     int64
	fetcher Fetcher
	ttl     time.Duration
	buckets []*Bucket
}

type Bucket struct {
	sync.RWMutex
	lookup map[Id]*Item
}

type Item struct {
	expires time.Time
	value   []byte
}

func newCache(fetcher Fetcher, configuration *Configuration) *Cache {
	cache := &Cache{
		fetcher: fetcher,
		ttl:     configuration.cacheTTL,
		max:     configuration.cacheSize,
		buckets: make([]*Bucket, BUCKET_COUNT),
	}
	for i := 0; i < BUCKET_COUNT; i++ {
		cache.buckets[i] = &Bucket{
			lookup: make(map[Id]*Item),
		}
	}
	return cache
}

func (c *Cache) Fill(result *NormalResult) error {
	missCount := 0
	misses := result.misses
	payloads := result.payloads
	for i, id := range result.Ids() {
		resource := c.get(id)
		if resource == nil {
			misses[missCount] = i
			missCount++
			misses[missCount] = id
			missCount++
		} else {
			payloads[i] = resource
		}
	}
	if missCount > 0 {
		if err := c.fetcher.Fill(misses[:missCount], payloads); err != nil {
			return err
		}
		for i := 0; i < missCount; i += 2 {
			c.set(misses[i+1].(Id), payloads[misses[i].(int)])
		}
	}
	return nil
}

func (c *Cache) Fetch(id Id) []byte {
	payload := c.get(id)
	if payload == nil {
		if payload = c.fetcher.Get(id); payload != nil {
			c.set(id, payload)
		}
	}
	return payload
}

func (c *Cache) get(id Id) []byte {
	bucket := c.bucket(id)
	item := bucket.get(id)
	if item == nil {
		return nil
	}
	if item.expires.After(time.Now()) {
		return item.value
	}
	if bucket.remove(id) == true {
		atomic.AddInt64(&c.size, int64(len(item.value)))
	}
	return nil
}

func (c *Cache) set(id Id, value []byte) {
	item := &Item{
		value:   value,
		expires: time.Now().Add(c.ttl),
	}
	if c.bucket(id).set(id, item) == true {
		if atomic.AddInt64(&c.size, int64(len(value))) >= c.max && atomic.CompareAndSwapUint32(&c.gcing, 0, 1) {
			go c.gc()
		}
	}
}

func (c *Cache) bucket(id Id) *Bucket {
	return c.buckets[id&BUCKET_MASK]
}

func (c *Cache) gc() {
	defer atomic.StoreUint32(&c.gcing, 0)
	freed := int64(0)
	for i := 0; i < BUCKET_COUNT; i++ {
		freed += c.buckets[i].gc()
	}
	atomic.AddInt64(&c.size, -freed)
}

func (b *Bucket) get(id Id) *Item {
	b.RLock()
	value := b.lookup[id]
	b.RUnlock()
	return value
}

func (b *Bucket) remove(id Id) bool {
	b.Lock()
	_, exists := b.lookup[id]
	delete(b.lookup, id)
	b.Unlock()
	return exists
}

func (b *Bucket) set(id Id, item *Item) bool {
	b.Lock()
	_, exists := b.lookup[id]
	b.lookup[id] = item
	b.Unlock()
	return !exists
}

func (b *Bucket) gc() int64 {
	visited := 0
	oldest := nullItem
	var oldestId Id

	b.RLock()
	for id, item := range b.lookup {
		if item.expires.Before(oldest.expires) {
			oldestId = id
			oldest = item
		}
		if visited++; visited == 10 {
			break
		}
	}
	b.RUnlock()

	b.Lock()
	delete(b.lookup, oldestId)
	b.Unlock()
	return int64(len(oldest.value))
}