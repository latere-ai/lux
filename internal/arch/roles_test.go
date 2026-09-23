// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"slices"
	"strings"
	"testing"
)

// tokenRole is the token role's allow list (spec 035): the build list of
// internal/token reads the configuration and signs, so it reaches the
// configuration package and what that package parses with, the local
// issuer's key, and nothing that opens a store, writes a journal row,
// delivers an event, or verifies a token. Its module packages are named
// one by one, since internal/store is the store's interface and every
// package under it is an implementation that opens something. The
// external rows are what internal/config reaches through
// latere.ai/x/pkg/authz, which parses LUX_ADMIN_SUBJECTS, and through
// internal/store's instrumentation types, which name the OpenTelemetry
// API; none of them is constructed by the role.
var tokenRole = struct {
	module   []string // exact import paths inside this module
	external []string // import path prefixes outside it
	forbid   []string // prefixes no row admits, whatever it says
	noStd    []string // standard library packages the role may not reach
}{
	module: []string{
		module + "/internal/token",
		module + "/internal/config",
		module + "/internal/localissuer",
		module + "/internal/secrets",
		module + "/internal/store",
		module + "/manifest/v1",
		module + "/metering",
	},
	external: []string{
		"latere.ai/x/pkg/authz",
		"latere.ai/x/pkg/authkit",
		"latere.ai/x/pkg/bearer",
		"latere.ai/x/pkg/cache",
		"latere.ai/x/pkg/envutil",
		"latere.ai/x/pkg/httpjson",
		"latere.ai/x/pkg/metrics",
		"go.opentelemetry.io/",
		"github.com/go-logr/",
		"github.com/cespare/xxhash",
		"github.com/google/uuid",
	},
	// The verifier would fetch an issuer's key set, the SDK exports, and
	// the drivers open a database: a role that signs one token needs
	// none of them.
	forbid: []string{
		"latere.ai/x/pkg/authkit/jwt",
		"go.opentelemetry.io/otel/sdk",
		"go.opentelemetry.io/otel/exporters",
		"github.com/jackc/",
		"github.com/golang-migrate/",
	},
	noStd: []string{"database/sql", "os/exec"},
}

// TestTokenRoleReachesNothingThatWrites holds the token role to its
// allow list over its whole build list, so the promise that luxd token
// writes nothing is a property of what it can reach and not only of
// what it happens to call today.
func TestTokenRoleReachesNothingThatWrites(t *testing.T) {
	dir := root(t)
	for _, line := range goList(t, dir, "-deps", "-f", "{{.ImportPath}} {{.Standard}}", "./internal/token") {
		path, std, _ := strings.Cut(line, " ")
		switch {
		case std == "true":
			if slices.Contains(tokenRole.noStd, path) {
				t.Errorf("the token role reaches %s", path)
			}
		case hasPrefix(path, tokenRole.forbid):
			t.Errorf("the token role reaches %s, which no row admits", path)
		case strings.HasPrefix(path, module+"/"):
			if !slices.Contains(tokenRole.module, path) {
				t.Errorf("the token role reaches %s, which its allow list does not name; a new package is a row in tokenRole with its reason", path)
			}
		case !hasPrefix(path, tokenRole.external):
			t.Errorf("the token role reaches %s, which its allow list does not name; a new dependency is a row in tokenRole with its reason", path)
		}
	}
}
