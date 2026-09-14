// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package stubs is the root of the stubs tree of spec 015. The stubs
// themselves live in the packages below it: provider, sink, and the two
// wrappers issuer and authorizer over latere.ai/x/pkg. This package holds
// no code of its own; its tests are the tiers' rules, which read the
// whole tree: every test in a tagged file carries its tier's name prefix,
// and the postgres tier refuses to run without its database rather than
// skipping.
package stubs
