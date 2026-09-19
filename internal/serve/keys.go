// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The Key cache of spec 007: what makes the hot path touch the store
// once per Key per window rather than once per request.
const (
	// KeyCacheEntries is the most entries a replica's cache holds,
	// positive and negative together; past it the least recent is
	// evicted.
	KeyCacheEntries = 100_000
	// MetricKeyCacheHits counts every lookup by result: hit for a Key
	// served from the cache, miss for one read from the store, negative
	// for an unknown value answered from a negative entry.
	MetricKeyCacheHits = "lux_key_cache_hits_total"
	// DefaultKeyCache is LUX_KEY_CACHE's default.
	DefaultKeyCache = 10 * time.Second
)

// The journal events that drop a cached entry: a Key's update, rotation,
// or deletion, and a Budget's update or deletion, as spec 012's table
// names them and spec 011's routes raise them.
const (
	eventKeyUpdated    = events.KeyUpdated
	eventKeyRotated    = events.KeyRotated
	eventKeyDeleted    = events.KeyDeleted
	eventBudgetUpdated = events.BudgetUpdated
	eventBudgetDeleted = events.BudgetDeleted
)

// KeyCacheOptions is what the cache runs under.
type KeyCacheOptions struct {
	Store store.Store
	// TTL is LUX_KEY_CACHE: how long a positive or negative entry is
	// served, measured from the start of its store read; zero is DefaultKeyCache.
	TTL time.Duration
	// Tail is how often Run reads the journal; zero is one second.
	Tail time.Duration
	// Metrics receives MetricKeyCacheHits; nil records none.
	Metrics *metrics.Registry
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// KeyCache is the door's Key lookup on one replica, spec 004's KeyLookup
// satisfied from the store's hash index: the resolved Key by the SHA-256
// of its value, and the Budget a Key draws from by id, each cached for
// the window with the absence of one cached the same way. A positive
// entry is also dropped when the journal names its object in a
// key.updated, key.rotated, key.deleted, budget.updated, or
// budget.deleted row, which Tail reads on every replica, so a delete, a
// disable, or a rotation is seen at the next request on a replica that
// has consumed the row and within the window on one that has not. In
// file mode the SIGHUP re-read has no journal row, and luxd calls Reset
// after it instead.
type KeyCache struct {
	o    KeyCacheOptions
	hits *metrics.Counter

	mu         sync.Mutex
	entries    map[string]*entry // by cache key
	lru        lru               // most recently used at the front
	byObject   map[string]map[string]bool
	after      int64  // the journal sequence Tail has read up to
	generation uint64 // invalidates reads whose object is not yet known
	sequence   uint64 // orders concurrent reads even when Now is unchanged
}

// entry is one cached answer: a Key, a Budget, or neither for a value no
// Key has and an id no Budget has. It is also a node of the recency
// list.
type entry struct {
	key        string
	id         string
	k          *v1.Key
	b          *v1.Budget
	expires    time.Time
	sequence   uint64
	prev, next *entry
}

// lru is the recency order of the entries, most recent at the front, as
// a doubly linked list of the entries themselves.
type lru struct {
	front, back *entry
	n           int
}

func (l *lru) pushFront(e *entry) {
	e.prev, e.next = nil, l.front
	if l.front != nil {
		l.front.prev = e
	}
	l.front = e
	if l.back == nil {
		l.back = e
	}
	l.n++
}

func (l *lru) remove(e *entry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.front = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.back = e.prev
	}
	e.prev, e.next = nil, nil
	l.n--
}

func (l *lru) moveToFront(e *entry) {
	if l.front == e {
		return
	}
	l.remove(e)
	l.pushFront(e)
}

// The cache key prefixes: a Key by hash, a Budget by id.
const (
	cacheKeyHash   = "k:"
	cacheKeyBudget = "b:"
)

// NewKeyCache constructs the cache; Run starts its journal tail.
func NewKeyCache(o KeyCacheOptions) *KeyCache {
	if o.TTL <= 0 {
		o.TTL = DefaultKeyCache
	}
	if o.Tail <= 0 {
		o.Tail = defaultTail
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &KeyCache{o: o, entries: map[string]*entry{}, byObject: map[string]map[string]bool{}}
	if o.Metrics != nil {
		c.hits = o.Metrics.Counter(MetricKeyCacheHits, "Key lookups by result: hit, miss, or negative.")
	}
	return c
}

// ByHash implements gateway.KeyLookup: the Key whose value's SHA-256 is
// hash, nil and no error for a value no Key has, and the store's own
// failure otherwise, which is never cached. The Key returned is the
// cache's copy and is read, never changed, by the caller.
func (c *KeyCache) ByHash(ctx context.Context, hash string) (*v1.Key, error) {
	e, hit, err := c.lookup(ctx, cacheKeyHash+hash, func() (*entry, error) { return c.readKey(ctx, hash) })
	result := "miss"
	if hit {
		result = "hit"
		if e.k == nil {
			result = "negative"
		}
	}
	c.count(result)
	if err != nil {
		return nil, err
	}
	return e.k, nil
}

func (c *KeyCache) readKey(ctx context.Context, hash string) (*entry, error) {
	id, err := c.o.Store.Keys().ByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return &entry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up the Key by hash: %w", err)
	}
	obj, _, err := c.o.Store.Objects().Get(ctx, v1.KindKey, id)
	if errors.Is(err, store.ErrNotFound) {
		return &entry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Key %s: %w", id, err)
	}
	k, ok := obj.(*v1.Key)
	if !ok {
		return nil, fmt.Errorf("reading Key %s: the store returned a %T", id, obj)
	}
	return &entry{id: id, k: k}, nil
}

// Budget is the Budget with id, cached under the same window and dropped
// by the same tail, nil and no error for an id no live Budget has, and
// the store's own failure otherwise. It is what the Limiter reads for a
// Key that draws from one, so a hard Budget's amount is a cache read and
// not a store round trip per request.
func (c *KeyCache) Budget(ctx context.Context, id string) (*v1.Budget, error) {
	e, _, err := c.lookup(ctx, cacheKeyBudget+id, func() (*entry, error) { return c.readBudget(ctx, id) })
	if err != nil {
		return nil, err
	}
	return e.b, nil
}

func (c *KeyCache) readBudget(ctx context.Context, id string) (*entry, error) {
	obj, _, err := c.o.Store.Objects().Get(ctx, v1.KindBudget, id)
	if errors.Is(err, store.ErrNotFound) {
		return &entry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Budget %s: %w", id, err)
	}
	b, ok := obj.(*v1.Budget)
	if !ok {
		return nil, fmt.Errorf("reading Budget %s: the store returned a %T", id, obj)
	}
	return &entry{id: id, b: b}, nil
}

// lookup never returns a store answer invalidated while it was in flight.
// TTL starts before the read, so a delayed result cannot extend authority.
// Sustained invalidation or a store slower than TTL fails closed after three reads.
func (c *KeyCache) lookup(ctx context.Context, key string, read func() (*entry, error)) (*entry, bool, error) {
	for attempt := range 3 {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if e, ok := c.get(key); ok {
			return e, attempt == 0, nil
		}
		c.mu.Lock()
		generation := c.generation
		c.sequence++
		sequence := c.sequence
		expires := c.o.Now().Add(c.o.TTL)
		c.mu.Unlock()
		e, err := read()
		if err != nil {
			return nil, false, err
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		e.key, e.expires, e.sequence = key, expires, sequence
		if current, ok := c.publish(e, generation); ok {
			return current, false, nil
		}
	}
	return nil, false, errors.New("reading cached authority: concurrent invalidation or expired lookup")
}

// count records one lookup's result.
func (c *KeyCache) count(result string) {
	if c.hits != nil {
		c.hits.Inc(map[string]string{"result": result})
	}
}

// get is a fresh entry, moved to the front, or nothing; a stale entry
// is dropped on the way.
func (c *KeyCache) get(key string) (*entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.o.Now().Before(e.expires) {
		c.remove(e)
		return nil, false
	}
	c.lru.moveToFront(e)
	return e, true
}

// publish accepts only a still-current read, preserving a newer cached answer.
func (c *KeyCache) publish(e *entry, generation uint64) (*entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation || !c.o.Now().Before(e.expires) {
		return nil, false
	}
	if old, ok := c.entries[e.key]; ok {
		if old.sequence > e.sequence && c.o.Now().Before(old.expires) {
			c.lru.moveToFront(old)
			return old, true
		}
		c.remove(old)
	}
	c.entries[e.key] = e
	c.lru.pushFront(e)
	if e.id != "" {
		keys := c.byObject[e.id]
		if keys == nil {
			keys = map[string]bool{}
			c.byObject[e.id] = keys
		}
		keys[e.key] = true
	}
	for c.lru.n > KeyCacheEntries {
		c.remove(c.lru.back)
	}
	return e, true
}

// remove drops one entry and its index rows. The caller holds the lock.
func (c *KeyCache) remove(e *entry) {
	c.lru.remove(e)
	delete(c.entries, e.key)
	if e.id != "" {
		delete(c.byObject[e.id], e.key)
		if len(c.byObject[e.id]) == 0 {
			delete(c.byObject, e.id)
		}
	}
}

// evictObject drops every entry that holds the object with id.
func (c *KeyCache) evictObject(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	for key := range c.byObject[id] {
		if e, ok := c.entries[key]; ok {
			c.remove(e)
		}
	}
	delete(c.byObject, id)
}

// Reset empties the cache: the file mode's SIGHUP re-read swaps the
// snapshot without a journal row, so luxd calls this after it and the
// next request of every Key reads the new snapshot.
func (c *KeyCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.entries = map[string]*entry{}
	c.lru = lru{}
	c.byObject = map[string]map[string]bool{}
}

// Len is the number of entries held, positive and negative.
func (c *KeyCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.n
}

// Tail reads the journal since the last read and drops the entries of
// every Key and Budget the rows name as changed or gone. A store that
// cannot answer is logged and the entries lapse with the window.
func (c *KeyCache) Tail(ctx context.Context) {
	const batch = 500
	c.mu.Lock()
	after := c.after
	c.mu.Unlock()
	for {
		events, err := c.o.Store.Journal().Since(ctx, after, batch)
		if err != nil {
			c.o.Logger.ErrorContext(ctx, "key cache: reading the journal", "after", after, "err", err)
			return
		}
		for _, e := range events {
			after = e.GSeq
			switch e.Type {
			case eventKeyUpdated, eventKeyRotated, eventKeyDeleted, eventBudgetUpdated, eventBudgetDeleted:
				c.evictObject(e.ObjectID)
			}
		}
		c.mu.Lock()
		c.after = after
		c.mu.Unlock()
		if len(events) < batch {
			return
		}
	}
}

// Run tails the journal on the interval until ctx ends. The first read
// walks the journal from its start, which evicts nothing from a cache
// that is empty at start and places the tail at the end.
func (c *KeyCache) Run(ctx context.Context) {
	c.Tail(ctx)
	t := time.NewTicker(c.o.Tail)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Tail(ctx)
		}
	}
}
