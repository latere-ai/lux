// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/lux/gateway"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The store is the platform's, and this one is a map: desired state by
// kind, the hash of every Key's value, each Provider's credential, the
// spend counters the metering package folds over, and the records of
// the requests this process served. A real platform puts the same rows
// in its own database under its own transactions; what the gateway asks
// of it is the four small interfaces of gateway.Options and
// metering.CounterStore, which this file satisfies and nothing else in
// the example knows about.

// errNotFound is what a read of an object that is not there returns.
var errNotFound = errors.New("no such object")

// errVersionConflict is a write against a version that has moved.
var errVersionConflict = errors.New("the object changed since it was read")

// errValueTaken is a Key created with a value another Key already has.
var errValueTaken = errors.New("a Key with this value already exists")

// entry is one object and the version of its last write, which is the
// ETag the API answers with.
type entry struct {
	obj     v1.Object
	version int64
}

// store is the platform's desired state and counters, in memory.
type store struct {
	mu          sync.Mutex
	objects     map[string]map[string]entry // kind to id to entry
	hashes      map[string]string           // the SHA-256 of a Key's value to its id
	credentials map[string][]byte           // Provider id to its credential
	counters    map[string]int64            // the spend windows metering keys
	records     []metering.Record           // every request this process served
}

func newStore() *store {
	s := &store{
		objects:     map[string]map[string]entry{},
		hashes:      map[string]string{},
		credentials: map[string][]byte{},
		counters:    map[string]int64{},
	}
	for _, kind := range kinds {
		s.objects[kind] = map[string]entry{}
	}
	return s
}

// kinds are the four the contract serves, in the order an apply of
// several has to take: a Model names a Provider, a Key a Budget.
var kinds = []string{v1.KindProvider, v1.KindBudget, v1.KindModel, v1.KindKey}

// clone is one object copied, so a handler that fills in a status does
// not write into the stored row.
func clone(obj v1.Object) v1.Object {
	switch x := obj.(type) {
	case *v1.Provider:
		c := *x
		return &c
	case *v1.Model:
		c := *x
		return &c
	case *v1.Key:
		c := *x
		return &c
	case *v1.Budget:
		c := *x
		return &c
	}
	return obj
}

// get is the object of a kind by id.
func (s *store) get(kind, id string) (v1.Object, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.objects[kind][id]
	if !ok {
		return nil, 0, errNotFound
	}
	return clone(e.obj), e.version, nil
}

// byName is the object of a kind by its metadata name.
func (s *store) byName(kind, name string) (v1.Object, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.objects[kind] {
		if e.obj.Name() == name {
			return clone(e.obj), e.version, nil
		}
	}
	return nil, 0, errNotFound
}

// list is every object of a kind, by name ascending, which is the order
// the cursor pages through.
func (s *store) list(kind string) []v1.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]v1.Object, 0, len(s.objects[kind]))
	for _, e := range s.objects[kind] {
		out = append(out, clone(e.obj))
	}
	slices.SortFunc(out, func(a, b v1.Object) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// put writes the object at the version it was read at; version 0 is a
// create, and a name another live object holds is refused.
func (s *store) put(obj v1.Object, version int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, id := obj.Kind(), obj.ID()
	e, live := s.objects[kind][id]
	if live && e.version != version {
		return 0, errVersionConflict
	}
	for otherID, other := range s.objects[kind] {
		if otherID != id && other.obj.Name() == obj.Name() {
			return 0, fmt.Errorf("a %s named %q already exists", kind, obj.Name())
		}
	}
	next := version + 1
	stored := clone(obj)
	setVersion(stored, next)
	s.objects[kind][id] = entry{obj: stored, version: next}
	return next, nil
}

// remove deletes one object, its hash, and its credential.
func (s *store) remove(kind, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects[kind], id)
	delete(s.credentials, id)
	for hash, keyID := range s.hashes {
		if keyID == id {
			delete(s.hashes, hash)
		}
	}
}

// putHash registers the hash of a Key's value, refusing one another Key
// already holds, which is what keeps a supplied value unique across the
// installation.
func (s *store) putHash(id, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if other, ok := s.hashes[hash]; ok && other != id {
		return errValueTaken
	}
	for h, keyID := range s.hashes {
		if keyID == id && h != hash {
			delete(s.hashes, h)
		}
	}
	s.hashes[hash] = id
	return nil
}

// putCredential holds a Provider's credential. A platform seals it
// under its own key management; the example keeps the bytes in memory
// and returns them to nobody.
func (s *store) putCredential(id string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.credentials[id] = value
}

// ByHash implements gateway.KeyLookup: the Key whose value hashes to
// hash, nil for one no Key has.
func (s *store) ByHash(_ context.Context, hash string) (*v1.Key, error) {
	s.mu.Lock()
	id, ok := s.hashes[hash]
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}
	obj, _, err := s.get(v1.KindKey, id)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, _ := obj.(*v1.Key)
	return key, nil
}

// Model implements gateway.Catalog: the Model of that exact name.
func (s *store) Model(_ context.Context, name string) (*v1.Model, error) {
	obj, _, err := s.byName(v1.KindModel, name)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m, _ := obj.(*v1.Model)
	return m, nil
}

// Models implements gateway.Catalog: every Model, for a door's list.
func (s *store) Models(context.Context) ([]*v1.Model, error) {
	var out []*v1.Model
	for _, obj := range s.list(v1.KindModel) {
		if m, ok := obj.(*v1.Model); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// Provider implements gateway.Catalog: the Provider by prv_ id or name,
// which are the two ways a target refers to one.
func (s *store) Provider(_ context.Context, nameOrID string) (*v1.Provider, error) {
	obj, _, err := s.reference(v1.KindProvider, v1.PrefixProvider, nameOrID)
	if err != nil || obj == nil {
		return nil, err
	}
	p, _ := obj.(*v1.Provider)
	return p, nil
}

// Budget is the Budget by bud_ id or name, nil for one that is gone.
func (s *store) Budget(_ context.Context, nameOrID string) (*v1.Budget, error) {
	obj, _, err := s.reference(v1.KindBudget, v1.PrefixBudget, nameOrID)
	if err != nil || obj == nil {
		return nil, err
	}
	b, _ := obj.(*v1.Budget)
	return b, nil
}

// reference reads one object by id when the reference carries the
// kind's prefix and by name otherwise; nil and no error for one that
// does not exist.
func (s *store) reference(kind, prefix, nameOrID string) (v1.Object, int64, error) {
	if nameOrID == "" {
		return nil, 0, nil
	}
	var (
		obj     v1.Object
		version int64
		err     error
	)
	if strings.HasPrefix(nameOrID, prefix) {
		obj, version, err = s.get(kind, nameOrID)
	} else {
		obj, version, err = s.byName(kind, nameOrID)
	}
	if errors.Is(err, errNotFound) {
		return nil, 0, nil
	}
	return obj, version, err
}

// Credential implements gateway.CredentialSource: the value the gateway
// injects toward one Provider, and nil for a Provider that holds none.
func (s *store) Credential(_ context.Context, providerID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentials[providerID], nil
}

// Add implements metering.CounterStore: one window's delta, atomically.
func (s *store) Add(_ context.Context, key string, delta int64, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[key] += delta
	return s.counters[key], nil
}

// Read implements metering.CounterStore.
func (s *store) Read(_ context.Context, keys []string) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(keys))
	for _, k := range keys {
		out[k] = s.counters[k]
	}
	return out, nil
}

// append keeps one request's record. A platform sends these to its own
// ledger; the example keeps them so GET /v1/usage and GET /v1/requests
// have something to fold.
func (s *store) append(r metering.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

// redact empties the owner of every record that carries it and answers
// how many did.
func (s *store) redact(owner string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.records {
		if s.records[i].Owner == owner {
			s.records[i].Owner = ""
			n++
		}
	}
	return n
}

// recordsOf is every record the filters admit, newest first.
func (s *store) recordsOf(match func(metering.Record) bool) []metering.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]metering.Record, 0, len(s.records))
	for _, v := range slices.Backward(s.records) {
		if match == nil || match(v) {
			out = append(out, v)
		}
	}
	return out
}

// The Key value's shape, spec 007's: lux_ and forty characters of a
// 64-letter alphabet, the first twelve of which are the handle a person
// sees again.
const (
	mintedPrefix   = "lux_"
	suppliedPrefix = "sup_"
	prefixLength   = 12
	valueAlphabet  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
)

// mintKeyValue is one fresh Key value from a cryptographically secure
// source.
func mintKeyValue() (string, error) {
	var b [40]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", fmt.Errorf("minting a Key value: %w", err)
	}
	out := make([]byte, 0, len(mintedPrefix)+len(b))
	out = append(out, mintedPrefix...)
	for _, c := range b {
		out = append(out, valueAlphabet[c&63])
	}
	return string(out), nil
}

// hashValue is the index a door looks a presented value up by: SHA-256
// over the exact bytes, nothing trimmed, decoded, or parsed first.
func hashValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// keyPrefix is the handle: the first twelve characters of a minted
// value, and suppliedKeyPrefix of the hash of a supplied one.
func keyPrefix(value string, supplied bool) string {
	if supplied {
		return suppliedKeyPrefix(hashValue(value))
	}
	return value[:prefixLength]
}

// suppliedKeyPrefix is the handle of a Key whose value the caller
// supplied, as the value or as its hash alone: sup_ and eight
// characters of the hash, because the value's own first characters may
// be the same for every value the platform issues.
func suppliedKeyPrefix(hash string) string {
	return suppliedPrefix + hash[:prefixLength-len(suppliedPrefix)]
}

// cursor encodes a page's resume point: the kind and the name to
// continue after, so a cursor of another kind is refused rather than
// silently paging the wrong list.
func cursorOf(kind, name string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + name))
}

// afterCursor is the name a cursor resumes after, and false when it is
// not this kind's.
func afterCursor(kind, cursor string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", false
	}
	got, name, ok := strings.Cut(string(raw), "|")
	if !ok || got != kind {
		return "", false
	}
	return name, true
}

// The seams: the store is the gateway's Key lookup, catalog,
// credential source, and counter table, and nothing in the gateway
// knows it is a map.
var (
	_ gateway.KeyLookup        = (*store)(nil)
	_ gateway.Catalog          = (*store)(nil)
	_ gateway.CredentialSource = (*store)(nil)
	_ metering.CounterStore    = (*store)(nil)
)
