// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/postgres/pgtest"
	v1 "latere.ai/x/lux/manifest/v1"
)

func TestPostgresFenceTransactionOrdering(t *testing.T) {
	for _, first := range []string{"write", "fence"} {
		t.Run(first, func(t *testing.T) {
			url := pgtest.URL(t)
			openStore := func() *Store {
				s, _, err := Connect(t.Context(), Options{URL: url})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				return s
			}
			a, b := openStore(), openStore()
			key := &v1.Key{Metadata: v1.ObjectMeta{Name: "late"}, Status: v1.KeyStatus{ID: v1.NewID(v1.PrefixKey, time.Now(), nil), Owner: "issuer|owner"}}
			fence := store.KeyFence{Name: key.Name(), Owner: key.Status.Owner}
			held, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- a.Transact(t.Context(), func(tx store.Store) error {
					var err error
					if first == "write" {
						_, err = tx.Objects().Put(t.Context(), key, 0)
						if err == nil {
							err = tx.Keys().Put(t.Context(), key.ID(), strings.Repeat("a", 64))
						}
					} else {
						_, err = tx.KeyFences().Put(t.Context(), fence)
					}
					close(held)
					<-release
					return err
				})
			}()
			<-held
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			var err error
			if first == "write" {
				_, err = b.KeyFences().Put(ctx, fence)
			} else {
				_, err = b.Objects().Put(ctx, key, 0)
			}
			cancel()
			close(release)
			if firstErr := <-done; firstErr != nil {
				t.Fatal(firstErr)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("operation crossed held transaction: %v", err)
			}
			if _, err := b.KeyFences().Put(t.Context(), fence); err != nil {
				t.Fatal(err)
			}
			// New connection sees the committed fence, including after deletion.
			c := openStore()
			if _, err := c.KeyFences().Get(t.Context(), fence.Name); err != nil {
				t.Fatal(err)
			}
			if first == "write" {
				if err := c.Keys().Put(t.Context(), key.ID(), strings.Repeat("b", 64)); !errors.Is(err, store.ErrKeyFenced) {
					t.Fatal(err)
				}
				if err := c.Objects().Delete(t.Context(), v1.KindKey, key.ID()); err != nil {
					t.Fatal(err)
				}
			}
			key.Status.ID = v1.NewID(v1.PrefixKey, time.Now(), nil)
			if _, err := c.Objects().Put(t.Context(), key, 0); !errors.Is(err, store.ErrKeyFenced) {
				t.Fatal(err)
			}
		})
	}
}

func TestPostgresFenceRejectsStaleIsolation(t *testing.T) {
	s, _, err := Connect(t.Context(), Options{URL: pgtest.URL(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	err = s.Transact(t.Context(), func(tx store.Store) error {
		pg, ok := tx.(*Store)
		if !ok {
			return errors.New("unexpected transaction store")
		}
		if _, err := pg.tx.Exec(t.Context(), `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		_, err := tx.KeyFences().Put(t.Context(), store.KeyFence{Name: "stale", Owner: "owner"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "READ COMMITTED") {
		t.Fatal(err)
	}
}

func TestPostgresFenceStorageFailuresFailClosed(t *testing.T) {
	for _, fault := range []string{"missing fence table", "malformed object"} {
		t.Run(fault, func(t *testing.T) {
			dbURL := pgtest.URL(t)
			s, _, err := Connect(t.Context(), Options{URL: dbURL})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			key := &v1.Key{Metadata: v1.ObjectMeta{Name: "closed"}, Status: v1.KeyStatus{ID: v1.NewID(v1.PrefixKey, time.Now(), nil), Owner: "owner"}}
			if _, err := s.Objects().Put(t.Context(), key, 0); err != nil {
				t.Fatal(err)
			}
			fence := store.KeyFence{Name: key.Name(), Owner: key.Status.Owner}
			if _, err := s.KeyFences().Put(t.Context(), fence); err != nil {
				t.Fatal(err)
			}
			if fault == "missing fence table" {
				pgtest.Exec(t, dbURL, `DROP TABLE key_fences`)
				if _, err := s.KeyFences().Put(t.Context(), fence); err == nil {
					t.Fatal("fence write failed open")
				}
			} else {
				pgtest.Exec(t, dbURL, `UPDATE objects SET spec='{"spec":42}' WHERE id=$1`, key.ID())
			}
			key.Spec.Disabled = true
			if _, err := s.Objects().Put(t.Context(), key, key.Status.Version); err == nil {
				t.Fatal("object write failed open")
			}
		})
	}
}
