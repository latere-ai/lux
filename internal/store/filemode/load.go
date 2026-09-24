// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filemode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// keyShape is the shape the server mints a Key value in (spec 007), so a
// file-mode Key is indistinguishable from a minted one everywhere it is
// shown.
var keyShape = regexp.MustCompile(`^lux_[A-Za-z0-9_-]{40}$`)

// prefixLength is the length of status.prefix, the first characters of
// the value.
const prefixLength = 12

// kindOrder is the order objects resolve in: a Model names a Provider,
// a Key names a Model and a Budget.
var kindOrder = []string{v1.KindProvider, v1.KindBudget, v1.KindModel, v1.KindKey}

// The media type of each spelling. One spelling per format: a .yml file
// is not read.
var mediaByExt = map[string]string{".yaml": manifest.MediaYAML, ".json": manifest.MediaJSON}

// document is one decoded manifest and the file it came from, relative
// to the directory.
type document struct {
	path string
	obj  v1.Object
}

// nameKey is the index key of one object by kind and name.
func nameKey(kind, name string) string { return kind + "/" + name }

// snapshot is one read of the directory: the resolved objects in the
// order they are written, the Key hashes and the credential values by
// name, and the sets the Lookup answers from as they fill.
type snapshot struct {
	objects   []v1.Object
	hashes    map[string]string // Key name to its hash
	creds     map[string]string // Provider id to its credential value
	providers map[string]*v1.Provider
	budgets   map[string]*v1.Budget
	models    []*v1.Model
	summary   Summary
}

func newSnapshot(dir string, files int) *snapshot {
	return &snapshot{
		hashes:    map[string]string{},
		creds:     map[string]string{},
		providers: map[string]*v1.Provider{},
		budgets:   map[string]*v1.Budget{},
		summary:   Summary{Dir: dir, Files: files, Kinds: map[string]int{}},
	}
}

// ids is the index of the snapshot's declared objects, read after the
// swap so a Model that replaced a discovered one carries the kept id.
func (s *snapshot) ids() map[string]string {
	out := make(map[string]string, len(s.objects))
	for _, obj := range s.objects {
		out[nameKey(obj.Kind(), obj.Name())] = obj.ID()
	}
	return out
}

// The snapshot is the Lookup of its own resolve: a Provider or Budget by
// name or id among those already resolved, and the declared Models a
// selector matches. Every reference is allowed, because there is no
// authorizer to ask, and a missing one is not_found at the field.

func (s *snapshot) Provider(_ context.Context, nameOrID string) (*v1.Provider, error) {
	if p, ok := s.providers[nameOrID]; ok {
		return p, nil
	}
	return nil, manifest.ErrNotFound
}

func (s *snapshot) Budget(_ context.Context, nameOrID string) (*v1.Budget, error) {
	if b, ok := s.budgets[nameOrID]; ok {
		return b, nil
	}
	return nil, manifest.ErrNotFound
}

func (s *snapshot) Models(_ context.Context, selector string) ([]v1.ModelRef, error) {
	var refs []v1.ModelRef
	for _, m := range s.models {
		if manifest.Match(selector, m.Metadata.Name) {
			refs = append(refs, v1.ModelRef{ID: m.Status.ID, Name: m.Metadata.Name, Owner: m.Status.Owner, Labels: maps.Clone(m.Metadata.Labels)})
		}
	}
	return refs, nil
}

// readDir decodes every *.yaml and *.json file under dir and its
// subdirectories, sorted by path. A file that fails to decode ends the
// read with the file and the refusal.
func readDir(dir string) ([]document, error) {
	var docs []document
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		media, ok := mediaByExt[filepath.Ext(path)]
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		obj, err := manifest.Decode(body, media, manifest.Hint{})
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		docs = append(docs, document{path: rel, obj: obj})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(docs, func(a, b document) int { return strings.Compare(a.path, b.path) })
	return docs, nil
}

// load reads and resolves the directory into a snapshot against the
// running one, whose ids are prev, without writing anything.
func (s *Store) load(ctx context.Context, prev map[string]string) (*snapshot, error) {
	docs, err := readDir(s.opts.Dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]string{}
	byKind := map[string][]document{}
	for _, d := range docs {
		kind, name := d.obj.Kind(), d.obj.Name()
		if name != "" {
			if other, dup := seen[nameKey(kind, name)]; dup {
				return nil, fmt.Errorf("%s: %s %q is also declared in %s", d.path, kind, name, other)
			}
			seen[nameKey(kind, name)] = d.path
		}
		byKind[kind] = append(byKind[kind], d)
	}
	snap := newSnapshot(s.opts.Dir, len(docs))
	now := s.opts.now()
	for _, kind := range kindOrder {
		for _, d := range byKind[kind] {
			var existing v1.Object
			if id, ok := prev[nameKey(kind, d.obj.Name())]; ok {
				obj, _, err := s.mem.Objects().Get(ctx, kind, id)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
				existing = obj
			}
			resolved, err := manifest.Resolve(ctx, d.obj, manifest.Options{
				Actor:                 manifest.Actor{Subject: Subject},
				Lookup:                snap,
				Defaults:              s.opts.Defaults,
				Existing:              existing,
				FileMode:              true,
				AllowPrivateUpstreams: s.opts.AllowPrivateUpstreams,
				TunnelEnabled:         false,
				PublicURL:             s.opts.PublicURL,
				Now:                   s.opts.now,
			})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.path, err)
			}
			if err := snap.add(resolved.Object, d.path, existing, now, s.opts.getenv); err != nil {
				return nil, err
			}
		}
	}
	return snap, nil
}

// add fills the status the mode owns, id, owner, timestamps, credential,
// prefix, budget, reads the values the object names from the
// environment, and files the object where the later kinds look it up.
func (s *snapshot) add(obj v1.Object, path string, existing v1.Object, now time.Time, getenv func(string) string) error {
	kind, name := obj.Kind(), obj.Name()
	id, createdAt := "", now
	if existing != nil {
		id, createdAt = existing.ID(), createdAtOf(existing)
	}
	if id == "" {
		id = v1.NewID(prefixOf(kind), now, nil)
	}
	switch x := obj.(type) {
	case *v1.Provider:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, Subject, createdAt, now
		x.Status.Credential = &v1.CredentialStatus{}
		if c := x.Spec.Credential; c != nil {
			value, set := c.Value()
			if c.ValueFrom != nil {
				if value = getenv(c.ValueFrom.Env); value == "" {
					return fmt.Errorf("%s: Provider %q: credential variable %s is unset or empty", path, name, c.ValueFrom.Env)
				}
				set = true
			}
			if set {
				s.creds[id] = value
				c.ClearValue()
				x.Status.Credential = &v1.CredentialStatus{Set: true, Version: 1, UpdatedAt: now}
			}
		}
		s.providers[name], s.providers[id] = x, x
	case *v1.Budget:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, Subject, createdAt, now
		s.budgets[name], s.budgets[id] = x, x
	case *v1.Model:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, Subject, createdAt, now
		x.Status.Source = v1.SourceDeclared
		s.models = append(s.models, x)
	case *v1.Key:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, Subject, createdAt, now
		if x.Spec.ValueFrom == nil {
			return fmt.Errorf("%s: Key %q: spec.valueFrom.env is required in file mode, so the value comes from a variable and never from a file", path, name)
		}
		env := x.Spec.ValueFrom.Env
		value := getenv(env)
		if value == "" {
			return fmt.Errorf("%s: Key %q: variable %s is unset or empty", path, name, env)
		}
		if !keyShape.MatchString(value) {
			return fmt.Errorf("%s: Key %q: the value of %s is not of the shape lux_ followed by 40 characters of [A-Za-z0-9_-]", path, name, env)
		}
		sum := sha256.Sum256([]byte(value))
		s.hashes[name] = hex.EncodeToString(sum[:])
		x.Status.Prefix = value[:prefixLength]
		if x.Spec.Budget != "" {
			if b, ok := s.budgets[x.Spec.Budget]; ok {
				x.Status.Budget = &v1.BudgetRef{Name: b.Metadata.Name, ID: b.Status.ID}
			}
		}
		for _, ref := range x.Spec.Budgets {
			if b, ok := s.budgets[ref]; ok {
				x.Status.Budgets = append(x.Status.Budgets, v1.BudgetRef{Name: b.Metadata.Name, ID: b.Status.ID})
			}
		}
	}
	s.objects = append(s.objects, obj)
	s.summary.Kinds[kind]++
	return nil
}

func prefixOf(kind string) string {
	switch kind {
	case v1.KindProvider:
		return v1.PrefixProvider
	case v1.KindBudget:
		return v1.PrefixBudget
	case v1.KindModel:
		return v1.PrefixModel
	default:
		return v1.PrefixKey
	}
}

func createdAtOf(obj v1.Object) time.Time {
	var at time.Time
	switch x := obj.(type) {
	case *v1.Provider:
		at = x.Status.CreatedAt
	case *v1.Budget:
		at = x.Status.CreatedAt
	case *v1.Model:
		at = x.Status.CreatedAt
	case *v1.Key:
		at = x.Status.CreatedAt
	}
	return at
}

// unchanged reports whether two objects have one metadata and one spec
// in their JSON form, which is what lets a re-read leave an untouched
// file's row at its version, so an ETag moves only when the file did.
func unchanged(old, next v1.Object) bool {
	return reflect.DeepEqual(shape(old), shape(next))
}

func shape(obj v1.Object) map[string]any {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return map[string]any{"metadata": m["metadata"], "spec": m["spec"]}
}

// swap writes the snapshot over the running one in one Transact of the
// memory store: declared objects the directory no longer holds are
// deleted with their hash and, for a Provider, its discovered Models; a
// Provider whose dialect or baseURL changed loses its discovered Models
// too; every changed object of the snapshot is written at the version
// its row is at, every new one created, and an unchanged one left at its
// version; every Key's hash is put, because the variable may have been
// rotated under an unchanged file; and the deleted rows are pruned. The
// observed half of every kept row is untouched, because Put never
// writes it.
func (s *Store) swap(ctx context.Context, prev map[string]string, snap *snapshot) error {
	next := snap.ids()
	now := s.opts.now()
	return s.mem.Transact(ctx, func(tx store.Store) error {
		for k, id := range prev {
			if _, keep := next[k]; keep {
				continue
			}
			kind, name, _ := strings.Cut(k, "/")
			if kind == v1.KindProvider {
				if err := dropDiscovered(ctx, tx, id); err != nil {
					return err
				}
			}
			if kind == v1.KindKey {
				if err := tx.Keys().Delete(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
			if err := tx.Objects().Delete(ctx, kind, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("removing %s %q: %w", kind, name, err)
			}
		}
		for _, obj := range snap.objects {
			p, ok := obj.(*v1.Provider)
			if !ok {
				continue
			}
			id, had := prev[nameKey(v1.KindProvider, p.Metadata.Name)]
			if !had {
				continue
			}
			old, _, err := tx.Objects().Get(ctx, v1.KindProvider, id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if op, ok := old.(*v1.Provider); ok && (op.Spec.Dialect != p.Spec.Dialect || op.Spec.BaseURL != p.Spec.BaseURL) {
				if err := dropDiscovered(ctx, tx, id); err != nil {
					return err
				}
			}
		}
		for _, obj := range snap.objects {
			kind, name := obj.Kind(), obj.Name()
			var version int64
			write := true
			if id, had := prev[nameKey(kind, name)]; had {
				old, v, err := tx.Objects().Get(ctx, kind, id)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				version = v
				write = err != nil || !unchanged(old, obj)
			}
			if write {
				if _, err := tx.Objects().Put(ctx, obj, version); err != nil {
					return fmt.Errorf("%s %q: %w", kind, name, err)
				}
			}
			if k, ok := obj.(*v1.Key); ok {
				if err := tx.Keys().Put(ctx, k.Status.ID, snap.hashes[name]); err != nil {
					return fmt.Errorf("hash of Key %q: %w", name, err)
				}
			}
		}
		_, err := tx.Objects().Prune(ctx, now)
		return err
	})
}

// dropDiscovered deletes the discovered Models of one Provider.
func dropDiscovered(ctx context.Context, tx store.Store, providerID string) error {
	models, _, err := tx.Objects().List(ctx, v1.KindModel, store.Filter{Source: string(v1.SourceDiscovered), Provider: providerID}, store.Page{})
	if err != nil {
		return err
	}
	for _, m := range models {
		if err := tx.Objects().Delete(ctx, v1.KindModel, m.ID()); err != nil {
			return err
		}
	}
	return nil
}
