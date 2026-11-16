// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/lifecycle"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// noVerifier refuses every bearer; the sessions here never heartbeat.
type noVerifier struct{}

func (noVerifier) Verify(string) (auth.Caller, error) {
	return auth.Caller{}, errors.New("no verifier")
}

// TestSessionWriters drives close, the one writer of a session's state,
// through SessionTable: the first close ends the session and every later one
// finds it closed.
func TestSessionWriters(t *testing.T) {
	g := New(Options{
		Store: memory.New(), Verifier: noVerifier{}, Clients: gateway.NewClientSource(gateway.ClientOptions{Version: "test"}),
		Logger: slog.New(slog.DiscardHandler),
	})
	var mu sync.Mutex
	sessions := map[string]*session{}
	d := statemachinetest.Driver[lifecycle.SessionState]{
		Enter: func(t *testing.T, state lifecycle.SessionState) string {
			ctx, cancel := context.WithCancel(context.Background())
			s := &session{
				g: g, id: fmt.Sprintf("tun_lc%d", len(sessions)+1), provider: &v1.Provider{Status: v1.ProviderStatus{ID: "prv_lc"}},
				done: make(chan struct{}), ctx: ctx, cancel: cancel, lastBeat: time.Now(), state: lifecycle.SessionLive,
			}
			if state == lifecycle.SessionClosed && !s.close(ctx, "") {
				t.Fatal("the first close did not close the session")
			}
			mu.Lock()
			defer mu.Unlock()
			sessions[s.id] = s
			return s.id
		},
		Move: func(id string, to lifecycle.SessionState) error {
			mu.Lock()
			s := sessions[id]
			mu.Unlock()
			switch to {
			case lifecycle.SessionClosed:
				if !s.close(context.Background(), "test") {
					return fmt.Errorf("session %s is already closed: %w", id, statemachine.ErrIllegalTransition)
				}
				return nil
			case lifecycle.SessionLive:
				return fmt.Errorf("session %s: no writer opens a closed session: %w", id, statemachine.ErrIllegalTransition)
			}
			return fmt.Errorf("session %s: no writer: %w", id, statemachine.ErrIllegalTransition)
		},
		State: func(t *testing.T, id string) lifecycle.SessionState {
			mu.Lock()
			s := sessions[id]
			mu.Unlock()
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.state
		},
	}
	statemachinetest.Exhaustive(t, lifecycle.SessionTable, d)
	statemachinetest.Race(t, lifecycle.SessionTable, d)
}
