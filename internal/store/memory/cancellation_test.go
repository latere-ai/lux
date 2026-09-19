// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

func TestTransactCancellationRollsBack(t *testing.T) {
	s := memory.New()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := provider("canceled")
	err := s.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, p, 0); err != nil {
			return err
		}
		if err := tx.Keys().Put(ctx, p.ID(), strings.Repeat("a", 64)); err != nil {
			return err
		}
		if _, err := tx.Journal().Append(ctx, store.Event{ID: "evt_canceled", ObjectID: p.ID()}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("transaction committed after cancellation: %v", err)
	}
	if _, _, err := s.Objects().Get(t.Context(), v1.KindProvider, p.ID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("object survived rollback", err)
	}
	if _, err := s.Keys().ByHash(t.Context(), strings.Repeat("a", 64)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("hash survived rollback", err)
	}
	rows, err := s.Journal().Since(t.Context(), 0, 10)
	if err != nil || len(rows) != 0 {
		t.Fatal("journal survived rollback", rows, err)
	}
}

type checkedContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *checkedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestTransactCancellationWhileWaitingSkipsCallback(t *testing.T) {
	s := memory.New()
	held, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.Transact(t.Context(), func(store.Store) error { close(held); <-release; return nil })
	}()
	<-held
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &checkedContext{Context: ctx, checked: make(chan struct{})}
	done := make(chan error, 1)
	called := false
	go func() { done <- s.Transact(observed, func(store.Store) error { called = true; return nil }) }()
	<-observed.checked
	cancel()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled waiter called=%v error=%v", called, err)
	}
}
