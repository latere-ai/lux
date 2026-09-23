// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store/postgres"
	"latere.ai/x/lux/internal/store/postgres/pgtest"
)

// The postgres tier of spec 015: the same tree of cases with LUX_DB_URL
// set, plus the cases that only exist with a shared store. It is
// selected by the postgres tag and the TestPostgres prefix:
//
//	LUX_DB_URL=postgres://... go test -tags=postgres -run '^TestPostgres' -v ./...
//
// The database is a dependency of the tier, declared by the variable and
// never started here; each test gets a database of its own on the
// cluster the variable names, so a run leaves nothing behind.

// unsetDBURL is the one line an unset LUX_DB_URL fails with: the variable
// and one way to satisfy it.
const unsetDBURL = "LUX_DB_URL is unset and the postgres tier needs a database: docker run --rm -p 5432:5432 -e POSTGRES_PASSWORD=lux postgres:17-alpine, then LUX_DB_URL=postgres://postgres:lux@127.0.0.1:5432/postgres?sslmode=disable"

// TestMain refuses to run without LUX_DB_URL: a tier that silently skips
// is a tier nobody notices is not running. With one, the binaries are
// built once as the integration tier builds them.
func TestMain(m *testing.M) {
	if os.Getenv("LUX_DB_URL") == "" {
		_, _ = fmt.Fprintln(os.Stderr, unsetDBURL)
		os.Exit(1)
	}
	dir, err := build()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// postgresEnv is the shared part of every replica's environment: one
// database, a fast metering flush so a shared counter is visible within
// a second, and a Key cache long enough that only the journal tail can
// explain a refusal inside it.
func postgresEnv(dbURL string) map[string]string {
	return map[string]string{"LUX_DB_URL": dbURL, "LUX_METERING_FLUSH": "200ms", "LUX_KEY_CACHE": "30s"}
}

// usage reads GET /v1/keys/{name} and returns status.usage.total.requests
// and status.usage.total.spend, or -1 when the Key is gone.
func (s *stack) usage(t *testing.T, name string) (requests int64, spend string) {
	t.Helper()
	resp := do(t, http.MethodGet, s.gw.public+"/v1/keys/"+name, bearer(s.token), "")
	if resp.status == http.StatusNotFound {
		return -1, ""
	}
	if resp.status != http.StatusOK {
		t.Fatalf("GET /v1/keys/%s = %d %s", name, resp.status, resp.body)
	}
	doc := resp.json(t)
	total, _ := doc["status"].(map[string]any)["usage"].(map[string]any)["total"].(map[string]any)
	n, _ := total["requests"].(float64)
	sp, _ := total["spend"].(string)
	return int64(n), sp
}

// eventsOfType counts the sink's verified deliveries of one type.
func (s *stack) eventsOfType(t *testing.T, typ string) int {
	t.Helper()
	n := 0
	for _, e := range s.events(t) {
		var body map[string]any
		if err := json.Unmarshal(e.Body, &body); err != nil {
			t.Fatalf("an event body: %v", err)
		}
		if body["type"] == typ {
			n++
		}
	}
	return n
}

// TestPostgresTwoReplicas runs two luxd processes against one database
// and proves what a shared store gives: desired state applied through
// one replica serves on the other once its journal tail has read the
// row, which its catalog snapshot follows, a spend counter both replicas
// add to sums on both within a flush, the journal lease has one holder
// so two replicas deliver one event per mutation, a Key deleted through
// one replica is refused by the other through the journal tail well
// inside the cache window, and a restart keeps every object, the spend
// window, and an event the sink had not acknowledged, which the new
// process delivers.
func TestPostgresTwoReplicas(t *testing.T) {
	dbURL := pgtest.URL(t)
	stubs := startStubs(t)
	shared := postgresEnv(dbURL)
	a := startLuxd(t, serverEnv(t, stubs, shared))
	b := startLuxd(t, serverEnv(t, stubs, shared))
	token := mint(t, stubs, "dev")
	sa, sb := &stack{stubs: stubs, gw: a, token: token}, &stack{stubs: stubs, gw: b, token: token}
	for _, gw := range []*gateway{a, b} {
		waitFor(t, "the start-up line", 5*time.Second, gw.out.String, "state in Postgres at ")
		if strings.Contains(gw.out.String(), "WARN") || strings.Contains(gw.out.String(), "lux@") {
			t.Fatalf("the start-up lines:\n%s", gw.out.String())
		}
	}

	// Desired state is shared: applied through a, served by b once b's
	// tail has read the row. b's catalog is watched on its internal
	// listener, so no request of the Key is spent waiting.
	sa.provider(t, "openai", "openai", false, "")
	sa.model(t, "chat", "openai", "gpt-stub", true)
	key := sa.key(t, "dev", "*")
	eventually(t, "b's catalog holding the Model", 5*time.Second, func() bool {
		body := string(do(t, http.MethodGet, b.internal+"/metrics", nil, "").body)
		return strings.Contains(body, `lux_catalog_objects{kind="Model"}`) && !strings.Contains(body, `lux_catalog_objects{kind="Model"} 0`)
	})
	for i := range 3 {
		if resp := sa.chat(t, key, "chat", "hi", false); resp.status != http.StatusOK {
			t.Fatalf("request %d through a: %d %s %s", i, resp.status, resp.code(), resp.body)
		}
	}
	for i := range 2 {
		if resp := sb.chat(t, key, "chat", "hi", false); resp.status != http.StatusOK {
			t.Fatalf("request %d through b: %d %s %s", i, resp.status, resp.code(), resp.body)
		}
	}
	// A shared spend counter: five requests, three through one replica
	// and two through the other, read as five on both within a flush.
	eventually(t, "both replicas reading five requests", 10*time.Second, func() bool {
		na, _ := sa.usage(t, "dev")
		nb, _ := sb.usage(t, "dev")
		return na == 5 && nb == 5
	})
	_, spendA := sa.usage(t, "dev")
	_, spendB := sb.usage(t, "dev")
	if spendA == "" || spendA == "0" || spendA != spendB {
		t.Fatalf("the spend on a is %q and on b %q", spendA, spendB)
	}

	// One journal lease holder: two replicas, one delivery per mutation.
	eventually(t, "the key.created event", 15*time.Second, func() bool { return sa.eventsOfType(t, "key.created") >= 1 })
	time.Sleep(2 * time.Second)
	if n := sa.eventsOfType(t, "key.created"); n != 1 {
		t.Fatalf("key.created was delivered %d time(s) by two replicas, want 1", n)
	}

	// The journal tail: b served the Key and caches it for thirty
	// seconds; a deletes it; b refuses within seconds, which only the
	// tail explains.
	if resp := do(t, http.MethodDelete, a.public+"/v1/keys/dev", bearer(token), ""); resp.status != http.StatusNoContent && resp.status != http.StatusOK {
		t.Fatalf("DELETE /v1/keys/dev = %d %s", resp.status, resp.body)
	}
	started := time.Now()
	eventually(t, "b refusing the deleted Key", 10*time.Second, func() bool {
		return sb.chat(t, key, "chat", "hi", false).status == http.StatusUnauthorized
	})
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the refusal took %s, which is the cache lapsing and not the tail", took)
	}

	// A restart: a second Key with spend behind it, a Budget applied
	// while the sink refuses every delivery, both replicas stopped.
	key2 := sa.key(t, "dev2", "*")
	for range 2 {
		if resp := sa.chat(t, key2, "chat", "hi", false); resp.status != http.StatusOK {
			t.Fatalf("a request on dev2: %d %s", resp.status, resp.body)
		}
	}
	eventually(t, "dev2's spend flushed", 10*time.Second, func() bool { n, _ := sa.usage(t, "dev2"); return n == 2 })
	if resp := do(t, http.MethodPut, stubs.urls["sink"]+"/_fail", http.Header{"Content-Type": {"application/json"}}, `{"first":1000}`); resp.status != http.StatusNoContent {
		t.Fatalf("PUT /_fail = %d %s", resp.status, resp.body)
	}
	sa.apply(t, "budget", "team", budgetYAML("team"))
	time.Sleep(2 * time.Second) // the holder tries, the sink refuses, the row is deferred
	if n := sa.eventsOfType(t, "budget.created"); n != 0 {
		t.Fatalf("budget.created was delivered %d time(s) while the sink refused", n)
	}
	if code := a.stop(t, 30*time.Second); code != 0 {
		t.Fatalf("a exited %d", code)
	}
	if code := b.stop(t, 30*time.Second); code != 0 {
		t.Fatalf("b exited %d", code)
	}
	if resp := do(t, http.MethodPut, stubs.urls["sink"]+"/_fail", http.Header{"Content-Type": {"application/json"}}, `{"first":0}`); resp.status != http.StatusNoContent {
		t.Fatalf("PUT /_fail = %d %s", resp.status, resp.body)
	}

	c := startLuxd(t, serverEnv(t, stubs, shared))
	sc := &stack{stubs: stubs, gw: c, token: token}
	eventually(t, "the new process delivering the unacknowledged event", 30*time.Second, func() bool {
		return sc.eventsOfType(t, "budget.created") == 1
	})
	if resp := do(t, http.MethodGet, c.public+"/v1/budgets/team", bearer(token), ""); resp.status != http.StatusOK {
		t.Fatalf("the Budget after the restart: %d %s", resp.status, resp.body)
	}
	if n, _ := sc.usage(t, "dev"); n != -1 {
		t.Fatalf("the deleted Key is back after the restart")
	}
	if n, _ := sc.usage(t, "dev2"); n != 2 {
		t.Fatalf("dev2's spend window after the restart reads %d requests, want 2", n)
	}
	if resp := sc.chat(t, key2, "chat", "hi", false); resp.status != http.StatusOK {
		t.Fatalf("dev2 after the restart: %d %s", resp.status, resp.body)
	}
	if resp := sc.chat(t, key, "chat", "hi", false); resp.status != http.StatusUnauthorized {
		t.Fatalf("the deleted Key after the restart: %d %s", resp.status, resp.body)
	}
	eventually(t, "dev2's window carrying the request after the restart", 10*time.Second, func() bool { n, _ := sc.usage(t, "dev2"); return n == 3 })
	if code := c.stop(t, 30*time.Second); code != 0 {
		t.Fatalf("c exited %d", code)
	}
}

// TestPostgresCheckReadsTheCluster runs luxd check as a process against
// a database: over an empty one the store row answers, the migrations
// row says the schema is not applied and check applies nothing; over the
// schema a serve applied the three rows are ok and the db conns row
// carries the cluster's max_connections; and a pool the cluster cannot
// hold fails the db conns row with exit 1.
func TestPostgresCheckReadsTheCluster(t *testing.T) {
	dbURL := pgtest.URL(t)
	stubs := startStubs(t)
	env := serverEnv(t, stubs, postgresEnv(dbURL))
	check := func(extra map[string]string) (int, string) {
		t.Helper()
		vars := map[string]string{}
		for k, v := range env {
			vars[k] = v
		}
		for k, v := range extra {
			vars[k] = v
		}
		p := startProcess(t, luxdBin, vars, "check")
		deadline := time.Now().Add(30 * time.Second)
		for !p.exited() {
			if time.Now().After(deadline) {
				t.Fatalf("luxd check did not exit:\n%s", p.out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		return p.stop(t, time.Second), p.out.String()
	}
	code, out := check(nil)
	if code != 0 || !strings.Contains(out, "ok   store: the Postgres store at") || !strings.Contains(out, "ok   migrations: the schema is not applied yet") || !strings.Contains(out, "warn providers: not checked; the schema is not applied yet") {
		t.Fatalf("check over an empty database: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "lux@") || strings.Contains(out, "postgres://") {
		t.Fatalf("check echoed the URL:\n%s", out)
	}

	gw := startLuxd(t, env)
	if code := gw.stop(t, 30*time.Second); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
	code, out = check(nil)
	if code != 0 || !strings.Contains(out, "ok   migrations: the schema is at version "+fmt.Sprint(postgres.Highest)+", this build's highest") || !strings.Contains(out, "ok   db conns: LUX_DB_MAX_CONNS is 8, so each replica opens up to 9 connections") || !strings.Contains(out, "max_connections is ") {
		t.Fatalf("check over the applied schema: exit %d\n%s", code, out)
	}
	code, out = check(map[string]string{"LUX_DB_MAX_CONNS": "100"})
	if code != 1 || !strings.Contains(out, "fail db conns: ") || !strings.Contains(out, "one replica alone exceeds it") {
		t.Fatalf("check with a pool of 100: exit %d\n%s", code, out)
	}
}
