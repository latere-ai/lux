// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/filemode"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The fixtures of spec 007's tests: Keys with minted or supplied values,
// Budgets, and Models with a price, stored as the API of spec 011 will
// store them, through one Transact of the object, its hash, and its
// journal row.

// reasonRequest is the reason of an event the API raises.
const reasonRequest = "request"

// storeKey writes k with its value's hash and the key.created row.
func (h *harness) storeKey(t *testing.T, k *v1.Key, value string, supplied bool) *v1.Key {
	t.Helper()
	ctx := t.Context()
	if k.Status.ID == "" {
		k.Status.ID = v1.NewID(v1.PrefixKey, h.clock(), nil)
	}
	k.Status.Owner = subject
	k.Status.Prefix = KeyPrefix(value, supplied)
	if k.Status.Warnings == nil {
		k.Status.Warnings = []string{}
	}
	err := h.st.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, k, 0); err != nil {
			return err
		}
		if err := tx.Keys().Put(ctx, k.Status.ID, HashKeyValue(value)); err != nil {
			return err
		}
		return appendEvent(ctx, tx.Journal(), "key.created", reasonRequest, k, map[string]any{"prefix": k.Status.Prefix}, h.clock(), newEventID(h.clock))
	})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// key stores a Key with a minted value and returns it with the value.
func (h *harness) key(t *testing.T, name string, edit func(k *v1.Key)) (*v1.Key, string) {
	t.Helper()
	value, err := MintKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	k := &v1.Key{Metadata: v1.ObjectMeta{Name: name}, Spec: v1.KeySpec{Models: []string{"*"}}}
	if edit != nil {
		edit(k)
	}
	return h.storeKey(t, k, value, false), value
}

// budget stores a Budget of amount in currency over window.
func (h *harness) budget(t *testing.T, name, amount, currency string, window v1.Window, hard bool) *v1.Budget {
	t.Helper()
	m, err := v1.ParseMoney(amount)
	if err != nil {
		t.Fatal(err)
	}
	b := &v1.Budget{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.BudgetSpec{Amount: &m, Currency: currency, Window: window, Hard: &hard},
		Status:   v1.BudgetStatus{ID: v1.NewID(v1.PrefixBudget, h.clock(), nil), Owner: subject, Warnings: []string{}},
	}
	if _, err := h.st.Objects().Put(t.Context(), b, 0); err != nil {
		t.Fatal(err)
	}
	return b
}

// draws is the edit that makes a Key draw from b.
func draws(b *v1.Budget) func(k *v1.Key) {
	return func(k *v1.Key) {
		k.Spec.Budget = b.Metadata.Name
		k.Status.Budget = &v1.BudgetRef{Name: b.Metadata.Name, ID: b.Status.ID}
	}
}

// event appends one API-raised row about obj, as spec 011's routes will.
func (h *harness) event(t *testing.T, typ string, obj v1.Object) {
	t.Helper()
	if err := appendEvent(t.Context(), h.st.Journal(), typ, reasonRequest, obj, map[string]any{}, h.clock(), newEventID(h.clock)); err != nil {
		t.Fatal(err)
	}
}

// instrumented is the harness's store counted on a registry, so a test
// proves how often the store was reached.
func (h *harness) instrumented() (store.Store, *metrics.Registry) {
	reg := metrics.NewRegistry()
	return store.Instrument(h.st, reg), reg
}

// ops is the count of one store operation that answered.
func ops(reg *metrics.Registry, op string) uint64 {
	return reg.Counter(store.MetricOperations, "").Value(map[string]string{"op": op, "result": store.ResultOK})
}

// hits is the cache metric for one result.
func hits(reg *metrics.Registry, result string) uint64 {
	return reg.Counter(MetricKeyCacheHits, "").Value(map[string]string{"result": result})
}

func (h *harness) cache(st store.Store, reg *metrics.Registry, ttl time.Duration) *KeyCache {
	return NewKeyCache(KeyCacheOptions{Store: st, TTL: ttl, Tail: 5 * time.Millisecond, Metrics: reg, Logger: h.logger, Now: h.clock})
}

// TestKeyCache: a Key looked up once is served from the cache for the
// window with one store call, the store is asked again after it, every
// lookup counts once in lux_key_cache_hits_total with its result, the
// Key comes back as the store renders it, and the Budget a Key draws
// from is cached the same way.
func TestKeyCache(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	b := h.budget(t, "team", "10", "USD", v1.WindowMonth, true)
	k, value := h.key(t, "run-42", draws(b))
	st, reg := h.instrumented()
	c := h.cache(st, reg, DefaultKeyCache)
	hash := HashKeyValue(value)
	for i := range 50 {
		got, err := c.ByHash(ctx, hash)
		if err != nil || got == nil {
			t.Fatalf("lookup %d: %v, %v", i, got, err)
		}
		if got.Status.ID != k.Status.ID || got.Status.Prefix != value[:12] || got.Status.Owner != subject || got.Status.Budget == nil || got.Status.Budget.ID != b.Status.ID || got.Metadata.Name != "run-42" {
			t.Fatalf("the cached Key = %+v", got.Status)
		}
	}
	if n := ops(reg, "Keys.ByHash"); n != 1 {
		t.Fatalf("Keys.ByHash was called %d times for fifty lookups", n)
	}
	if hits(reg, "miss") != 1 || hits(reg, "hit") != 49 || hits(reg, "negative") != 0 {
		t.Fatalf("metric: miss %d hit %d negative %d", hits(reg, "miss"), hits(reg, "hit"), hits(reg, "negative"))
	}
	h.advance(DefaultKeyCache)
	if _, err := c.ByHash(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if n := ops(reg, "Keys.ByHash"); n != 2 {
		t.Fatalf("after the window, Keys.ByHash was called %d times", n)
	}
	// The Budget by id: one store read per window.
	for range 3 {
		got, err := c.Budget(ctx, b.Status.ID)
		if err != nil || got == nil || got.Status.ID != b.Status.ID || got.Spec.Amount == nil || *got.Spec.Amount != 10_000_000 {
			t.Fatalf("Budget = %+v, %v", got, err)
		}
	}
	if none, err := c.Budget(ctx, "bud_missing"); err != nil || none != nil {
		t.Fatalf("a missing Budget = %v, %v", none, err)
	}
	if _, err := c.Budget(ctx, "bud_missing"); err != nil {
		t.Fatal(err)
	}
	if n := ops(reg, "Objects.Get"); n != 2+1+1 {
		t.Fatalf("Objects.Get was called %d times: two Key reads, one Budget read, one missing Budget", n)
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d", c.Len())
	}
	// A cache with no registry records nothing and still answers.
	quiet := NewKeyCache(KeyCacheOptions{Store: st})
	if got, err := quiet.ByHash(ctx, hash); err != nil || got == nil || quiet.o.TTL != DefaultKeyCache || quiet.o.Tail != defaultTail {
		t.Fatalf("a cache with defaults: %v, %v, %+v", got, err, quiet.o)
	}
}

// TestNegativeCache: an unknown value costs one store call per window
// and is answered from a negative entry between, counted as negative.
func TestNegativeCache(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	c := h.cache(st, reg, DefaultKeyCache)
	hash := HashKeyValue("lux_no-such-value")
	for range 20 {
		got, err := c.ByHash(ctx, hash)
		if err != nil || got != nil {
			t.Fatalf("an unknown value = %v, %v", got, err)
		}
	}
	if n := ops(reg, "Keys.ByHash"); n != 1 {
		t.Fatalf("Keys.ByHash was called %d times for twenty unknown lookups", n)
	}
	if hits(reg, "miss") != 1 || hits(reg, "negative") != 19 || hits(reg, "hit") != 0 {
		t.Fatalf("metric: miss %d negative %d hit %d", hits(reg, "miss"), hits(reg, "negative"), hits(reg, "hit"))
	}
	h.advance(DefaultKeyCache + time.Millisecond)
	if _, err := c.ByHash(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if n := ops(reg, "Keys.ByHash"); n != 2 {
		t.Fatalf("after the window, Keys.ByHash was called %d times", n)
	}
	// A hash whose row is gone while the hash index still names it is
	// unknown too, and cached as such.
	k, value := h.key(t, "gone", nil)
	if err := h.st.Objects().Delete(ctx, v1.KindKey, k.Status.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := c.ByHash(ctx, HashKeyValue(value)); err != nil || got != nil {
		t.Fatalf("a hash without a row = %v, %v", got, err)
	}
	if got, err := c.ByHash(ctx, HashKeyValue(value)); err != nil || got != nil || ops(reg, "Objects.Get") != 1 {
		t.Fatalf("a hash without a row, again = %v, %v, %d reads", got, err, ops(reg, "Objects.Get"))
	}
}

// TestCacheBound: the cache holds 100 000 entries and evicts the least
// recent past it, so a flood of unknown values cannot grow it.
func TestCacheBound(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	c := h.cache(st, reg, time.Hour)
	first := HashKeyValue("value-0")
	for i := range KeyCacheEntries + 1 {
		if _, err := c.ByHash(ctx, HashKeyValue("value-"+strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != KeyCacheEntries {
		t.Fatalf("Len = %d, want %d", c.Len(), KeyCacheEntries)
	}
	before := ops(reg, "Keys.ByHash")
	if _, err := c.ByHash(ctx, first); err != nil {
		t.Fatal(err)
	}
	if ops(reg, "Keys.ByHash") != before+1 {
		t.Fatal("the least recent entry was not evicted")
	}
	// The second value is the least recent now and goes next; a recent
	// one stays.
	if _, err := c.ByHash(ctx, HashKeyValue("value-"+strconv.Itoa(KeyCacheEntries))); err != nil {
		t.Fatal(err)
	}
	before = ops(reg, "Keys.ByHash")
	if _, err := c.ByHash(ctx, HashKeyValue("value-1")); err != nil || ops(reg, "Keys.ByHash") != before+1 {
		t.Fatalf("value-1 was kept: %v", err)
	}
	if _, err := c.ByHash(ctx, first); err != nil || ops(reg, "Keys.ByHash") != before+1 {
		t.Fatalf("value-0, just read, was evicted: %v", err)
	}
	c.Reset()
	if c.Len() != 0 {
		t.Fatalf("Len after Reset = %d", c.Len())
	}
}

// TestRevocationPropagates: a deleted, disabled, or rotated Key is
// refused at the next request on a replica that has consumed the
// journal row and within the window on one that has not; a Budget's
// update is seen the same way.
func TestRevocationPropagates(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	a, b := h.cache(st, reg, DefaultKeyCache), h.cache(st, reg, DefaultKeyCache)
	a.Tail(ctx) // a tails; b never does

	// Delete.
	k, value := h.key(t, "deleted", nil)
	hash := HashKeyValue(value)
	for _, c := range []*KeyCache{a, b} {
		if got, err := c.ByHash(ctx, hash); err != nil || got == nil {
			t.Fatal("the Key opens nothing before its deletion")
		}
	}
	if err := h.st.Transact(ctx, func(tx store.Store) error {
		if err := tx.Objects().Delete(ctx, v1.KindKey, k.Status.ID); err != nil {
			return err
		}
		if err := tx.Keys().Delete(ctx, k.Status.ID); err != nil {
			return err
		}
		return appendEvent(ctx, tx.Journal(), eventKeyDeleted, reasonRequest, k, map[string]any{"prefix": k.Status.Prefix}, h.clock(), newEventID(h.clock))
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.ByHash(ctx, hash); got == nil {
		t.Fatal("the replica that has not consumed the row refused inside the window")
	}
	a.Tail(ctx)
	if got, err := a.ByHash(ctx, hash); err != nil || got != nil {
		t.Fatalf("the replica that consumed the row still serves the deleted Key: %v, %v", got, err)
	}
	h.advance(DefaultKeyCache)
	if got, err := b.ByHash(ctx, hash); err != nil || got != nil {
		t.Fatalf("after the window the other replica still serves the deleted Key: %v, %v", got, err)
	}

	// Disable: the cached Key is replaced by the store's, whose spec says
	// disabled, which the door refuses key_disabled.
	k, value = h.key(t, "disabled", nil)
	hash = HashKeyValue(value)
	if got, _ := a.ByHash(ctx, hash); got == nil || got.Spec.Disabled {
		t.Fatal("the Key is not served enabled")
	}
	k.Spec.Disabled = true
	if _, err := h.st.Objects().Put(ctx, k, k.Status.Version); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventKeyUpdated, k)
	if got, _ := a.ByHash(ctx, hash); got == nil || got.Spec.Disabled {
		t.Fatal("the row was seen before the tail read it")
	}
	a.Tail(ctx)
	if got, _ := a.ByHash(ctx, hash); got == nil || !got.Spec.Disabled {
		t.Fatal("the disabled Key is still served enabled after the tail")
	}

	// Rotate: the old hash opens nothing at once on the replica that
	// tailed; the new one opens the same Key.
	k, value = h.key(t, "rotated", nil)
	old := HashKeyValue(value)
	if got, _ := a.ByHash(ctx, old); got == nil {
		t.Fatal("the Key does not open before its rotation")
	}
	fresh, _ := MintKeyValue()
	if err := h.st.Keys().Put(ctx, k.Status.ID, HashKeyValue(fresh)); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventKeyRotated, k)
	a.Tail(ctx)
	if got, err := a.ByHash(ctx, old); err != nil || got != nil {
		t.Fatalf("the old value still opens after the rotation: %v, %v", got, err)
	}
	if got, err := a.ByHash(ctx, HashKeyValue(fresh)); err != nil || got == nil || got.Status.ID != k.Status.ID {
		t.Fatalf("the new value does not open the Key: %v, %v", got, err)
	}

	// A Budget's update drops its entry; an unrelated row drops nothing.
	bud := h.budget(t, "team", "10", "USD", v1.WindowMonth, true)
	if got, _ := a.Budget(ctx, bud.Status.ID); got == nil || *got.Spec.Amount != 10_000_000 {
		t.Fatal("the Budget is not served")
	}
	amount := v1.Money(20_000_000)
	bud.Spec.Amount = &amount
	if _, err := h.st.Objects().Put(ctx, bud, bud.Status.Version); err != nil {
		t.Fatal(err)
	}
	h.event(t, "budget.created", bud) // not an invalidating type
	a.Tail(ctx)
	if got, _ := a.Budget(ctx, bud.Status.ID); got == nil || *got.Spec.Amount != 10_000_000 {
		t.Fatal("an unrelated row dropped the entry")
	}
	h.event(t, eventBudgetUpdated, bud)
	a.Tail(ctx)
	if got, _ := a.Budget(ctx, bud.Status.ID); got == nil || *got.Spec.Amount != 20_000_000 {
		t.Fatal("the raised amount is not served after the tail")
	}
}

// TestKeyCacheRunTailsAndSurvivesFailures: Run tails on its interval and
// stops with the context; a journal that cannot be read is logged and
// the entries lapse with the window; a hash index or an object read
// that fails is the store's error, answered store_unavailable by the
// door and cached nowhere.
func TestKeyCacheRunTailsAndSurvivesFailures(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	k, value := h.key(t, "run", nil)
	hash := HashKeyValue(value)
	c := NewKeyCache(KeyCacheOptions{Store: h.st, TTL: time.Hour, Tail: 5 * time.Millisecond, Logger: h.logger, Now: h.clock})
	run(t, c.Run)
	if got, err := c.ByHash(ctx, hash); err != nil || got == nil {
		t.Fatal("the Key does not open")
	}
	if err := h.st.Objects().Delete(ctx, v1.KindKey, k.Status.ID); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventKeyDeleted, k)
	waitUntil(t, "the tail evicting the deleted Key", func() bool {
		got, _ := c.ByHash(ctx, hash)
		return got == nil
	})

	// The failures, against a cache whose tail is driven by hand so the
	// broken operations are toggled with nothing running beside.
	fail := map[string]bool{}
	st := &broken{Store: h.st, fail: fail}
	c = NewKeyCache(KeyCacheOptions{Store: st, TTL: time.Hour, Logger: h.logger, Now: h.clock})
	fail["Journal.Since"] = true
	c.Tail(ctx)
	if !strings.Contains(h.logged(), "key cache: reading the journal") {
		t.Fatalf("the broken journal was not logged:\n%s", h.logged())
	}
	fail["Journal.Since"] = false
	fail["Keys.ByHash"] = true
	if _, err := c.ByHash(ctx, HashKeyValue("other")); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "Key by hash") {
		t.Fatalf("a broken hash index = %v", err)
	}
	fail["Keys.ByHash"] = false
	k2, value2 := h.key(t, "read-fails", nil)
	fail["Objects.Get"] = true
	if _, err := c.ByHash(ctx, HashKeyValue(value2)); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "Key "+k2.Status.ID) {
		t.Fatalf("a broken object read = %v", err)
	}
	if _, err := c.Budget(ctx, "bud_x"); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "Budget bud_x") {
		t.Fatalf("a broken Budget read = %v", err)
	}
	fail["Objects.Get"] = false
	if got, err := c.ByHash(ctx, HashKeyValue(value2)); err != nil || got == nil {
		t.Fatalf("the failure was cached: %v, %v", got, err)
	}
	if strings.Contains(h.logged(), value) || strings.Contains(h.logged(), value2) {
		t.Fatal("a Key value reached the log")
	}
}

// keyManifest is a file-mode Key whose value comes from RUN_KEY.
const keyManifest = `apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: run-42
spec:
  models: ["*"]
  valueFrom:
    env: RUN_KEY
`

// TestFileModeReloadEmptiesTheCache: in file mode the SIGHUP re-read
// writes no journal row, so the cache is emptied instead and the next
// lookup reads the new snapshot.
func TestFileModeReloadEmptiesTheCache(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.yaml"), []byte(keyManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	first, _ := MintKeyValue()
	second, _ := MintKeyValue()
	var mu sync.Mutex
	env := map[string]string{"RUN_KEY": first}
	getenv := func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		return env[name]
	}
	files, err := filemode.Load(ctx, filemode.Options{Dir: dir, Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewRegistry()
	st := store.Instrument(files, reg)
	c := NewKeyCache(KeyCacheOptions{Store: st, TTL: time.Hour, Metrics: reg})
	for range 3 {
		if got, err := c.ByHash(ctx, HashKeyValue(first)); err != nil || got == nil || got.Metadata.Name != "run-42" {
			t.Fatalf("the file-mode Key does not open: %v, %v", got, err)
		}
	}
	if ops(reg, "Keys.ByHash") != 1 {
		t.Fatalf("Keys.ByHash was called %d times", ops(reg, "Keys.ByHash"))
	}
	// The variable is rotated under the unchanged file: after the
	// re-read and the reset, the old value opens nothing and the new one
	// opens the Key, each read from the snapshot once.
	mu.Lock()
	env["RUN_KEY"] = second
	mu.Unlock()
	if err := files.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.ByHash(ctx, HashKeyValue(first)); got == nil {
		t.Fatal("the cache emptied itself without a Reset")
	}
	c.Reset()
	if got, err := c.ByHash(ctx, HashKeyValue(first)); err != nil || got != nil {
		t.Fatalf("the old value opens after the re-read: %v, %v", got, err)
	}
	if got, err := c.ByHash(ctx, HashKeyValue(second)); err != nil || got == nil {
		t.Fatalf("the new value does not open after the re-read: %v, %v", got, err)
	}
	if ops(reg, "Keys.ByHash") != 3 {
		t.Fatalf("Keys.ByHash was called %d times", ops(reg, "Keys.ByHash"))
	}
}

// TestSuppliedKeyValue: a Key created with a 32-byte supplied value
// resolves by the hash of that exact value, and its prefix is sup_ and
// the first eight hex characters of the SHA-256.
func TestSuppliedKeyValue(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	value := "platform-token-0123456789abcdef!" // 32 bytes
	if len(value) != 32 {
		t.Fatalf("the fixture is %d bytes", len(value))
	}
	k := h.storeKey(t, &v1.Key{Metadata: v1.ObjectMeta{Name: "dev-1"}, Spec: v1.KeySpec{Models: []string{"*"}}}, value, true)
	st, reg := h.instrumented()
	c := h.cache(st, reg, DefaultKeyCache)
	got, err := c.ByHash(ctx, HashKeyValue(value))
	if err != nil || got == nil || got.Status.ID != k.Status.ID {
		t.Fatalf("the supplied value does not open the Key: %v, %v", got, err)
	}
	if got.Status.Prefix != SuppliedPrefix+HashKeyValue(value)[:8] || len(got.Status.Prefix) != 12 || got.Status.Value != "" {
		t.Fatalf("status = prefix %q value %q", got.Status.Prefix, got.Status.Value)
	}
	if v, set := got.Spec.Value(); set || v != "" {
		t.Fatal("the stored Key carries the supplied value")
	}
}

// TestSuppliedValueIsOpaqueBytes: a value with leading whitespace opens
// the Key only with that whitespace, and a well-formed JWT with a past
// exp and a bad signature opens it, because the gateway parses nothing.
func TestSuppliedValueIsOpaqueBytes(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	c := h.cache(st, reg, DefaultKeyCache)
	spaced := "   spaced-value-0123456789abcdefghijklmnop"
	h.storeKey(t, &v1.Key{Metadata: v1.ObjectMeta{Name: "spaced"}}, spaced, true)
	if got, _ := c.ByHash(ctx, HashKeyValue(spaced)); got == nil {
		t.Fatal("the exact bytes do not open the Key")
	}
	if got, _ := c.ByHash(ctx, HashKeyValue(strings.TrimSpace(spaced))); got != nil {
		t.Fatal("the trimmed value opens the Key")
	}
	// {"alg":"RS256"}.{"exp":1,"aud":"other"}.not-a-signature
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjEsImF1ZCI6Im90aGVyIn0.bm90LWEtc2lnbmF0dXJl"
	jk := h.storeKey(t, &v1.Key{Metadata: v1.ObjectMeta{Name: "jwt"}}, jwt, true)
	if got, err := c.ByHash(ctx, HashKeyValue(jwt)); err != nil || got == nil || got.Status.ID != jk.Status.ID {
		t.Fatalf("the expired, unsigned token does not open the Key it was registered as: %v, %v", got, err)
	}
	if got, _ := c.ByHash(ctx, HashKeyValue(jwt+" ")); got != nil {
		t.Fatal("a value that differs by one byte opens the Key")
	}
}

// TestSuppliedValueMustBeUnique: a second create with a value already
// registered, to a live or a disabled Key, is ErrHashTaken inside the
// create's transaction, so nothing of the second Key is stored; two
// concurrent creates of one value yield one Key.
func TestSuppliedValueMustBeUnique(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	value := "shared-platform-token-0123456789"
	create := func(name string) error {
		k := &v1.Key{Metadata: v1.ObjectMeta{Name: name}, Status: v1.KeyStatus{ID: v1.NewID(v1.PrefixKey, h.clock(), nil), Owner: subject, Prefix: KeyPrefix(value, true), Warnings: []string{}}}
		return h.st.Transact(ctx, func(tx store.Store) error {
			if _, err := tx.Objects().Put(ctx, k, 0); err != nil {
				return err
			}
			return tx.Keys().Put(ctx, k.Status.ID, HashKeyValue(value))
		})
	}
	if err := create("first"); err != nil {
		t.Fatal(err)
	}
	err := create("second")
	if !errors.Is(err, store.ErrHashTaken) {
		t.Fatalf("a second create = %v", err)
	}
	if strings.Contains(err.Error(), "first") || strings.Contains(err.Error(), "key_") {
		t.Fatalf("the refusal names the Key that holds the value: %v", err)
	}
	if _, _, err := h.st.Objects().ByName(ctx, v1.KindKey, "second"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused Key was stored: %v", err)
	}
	// Disabled, the value is still taken.
	obj, version, err := h.st.Objects().ByName(ctx, v1.KindKey, "first")
	if err != nil {
		t.Fatal(err)
	}
	first := obj.(*v1.Key)
	first.Spec.Disabled = true
	if _, err := h.st.Objects().Put(ctx, first, version); err != nil {
		t.Fatal(err)
	}
	if err := create("third"); !errors.Is(err, store.ErrHashTaken) {
		t.Fatalf("a create over a disabled Key's value = %v", err)
	}
	// Two concurrent creates of one fresh value: one Key.
	value = "another-platform-token-0123456789"
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { results[i] = create("racer-" + strconv.Itoa(i)) })
	}
	wg.Wait()
	created := 0
	for _, err := range results {
		switch {
		case err == nil:
			created++
		case !errors.Is(err, store.ErrHashTaken):
			t.Fatalf("a concurrent create = %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("%d Keys were created for one value", created)
	}
}

// brokenKeys and brokenCounters extend the broken store to the hash
// index and the counter table.
func (b *broken) Keys() store.Keys         { return brokenKeys{b.Store.Keys(), b.fail} }
func (b *broken) Counters() store.Counters { return brokenCounters{b.Store.Counters(), b.fail} }

type brokenKeys struct {
	store.Keys
	fail map[string]bool
}

func (k brokenKeys) ByHash(ctx context.Context, hash string) (string, error) {
	if k.fail["Keys.ByHash"] {
		return "", errBroken
	}
	return k.Keys.ByHash(ctx, hash)
}

type brokenCounters struct {
	store.Counters
	fail map[string]bool
}

func (c brokenCounters) Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	if c.fail["Counters.Add"] || (strings.Contains(key, ":exhausted:") && c.fail["Counters.AddMarker"]) {
		return 0, errBroken
	}
	return c.Counters.Add(ctx, key, delta, expiresAt)
}

func (c brokenCounters) Read(ctx context.Context, keys []string) (map[string]int64, error) {
	if c.fail["Counters.Read"] {
		return nil, errBroken
	}
	return c.Counters.Read(ctx, keys)
}

// TestKeyLookupComparesNothing: the path from a door to the store hashes
// the presented value and asks an index for the hash; no file on that
// path compares a presented value with a stored one, in constant time or
// otherwise, so there is no comparison whose timing could leak a value.
func TestKeyLookupComparesNothing(t *testing.T) {
	forbidden := map[string]string{
		`"crypto/subtle"`:            "import",
		`"crypto/hmac"`:              "import",
		`subtle.ConstantTimeCompare`: "call",
		`hmac.Equal`:                 "call",
		`bytes.Equal`:                "call",
	}
	files := []string{"../../gateway/key.go", "keys.go", "keyvalue.go"}
	entries, err := os.ReadDir("../store/memory")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			files = append(files, "../store/memory/"+e.Name())
		}
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for needle, kind := range forbidden {
			if strings.Contains(string(data), needle) {
				t.Errorf("%s: %s %s on the key lookup path", f, kind, needle)
			}
		}
	}
	if HashKeyValue("lux_x") != fmt.Sprintf("%x", sha256.Sum256([]byte("lux_x"))) {
		t.Error("the door's hash is not SHA-256 of the exact bytes")
	}
}
