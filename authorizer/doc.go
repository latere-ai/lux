// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package authorizer is the vocabulary an authorization endpoint for Lux
// is written against: the actions luxd asks, the resource it sends with
// each one, and the limits an allow may carry. Import it to write the
// endpoint LUX_AUTHORIZER_URL points at, in Go, instead of keeping a
// copy of the strings.
//
// The envelope on the wire is latere.ai/x/pkg/authz's: luxd POSTs a
// request with the caller's subject, its claims, an action, and a
// resource, and reads back an allow or a deny. This package is the Lux
// half of that contract. Actions lists every action, twenty-five of
// them, each one a constant here; Kind reports the resource kind an
// action acts on, and Known whether a string is in the vocabulary at
// all. Nothing here dials: the package builds values and decodes them,
// and the client is the caller's.
//
// Vocabulary is that table in one value, of the shared contract's own
// type: the twenty-five actions in the spec's order, each paired with
// the kind it acts on. It is what an endpoint validates a request
// against and what latere.ai/x/pkg/authz/conformance drives its cases
// from, and Actions, Kind, and Known read the same value, so a consumer
// holds one declaration and never a copy of it:
//
//	http.Handle("POST /authorize", server.New(server.Options{
//		Bearer:     os.Getenv("AUTHORIZER_TOKEN"),
//		Vocabulary: authorizer.Vocabulary(),
//		Decider:    policy{},
//	}))
//
// Lux's four list actions answer a decision like every other action,
// narrowed by the answer's filter rather than returning a page of their
// own, so an endpoint for Lux names no page action.
//
// One resource shape goes with each action. The builders render exactly
// the fields luxd sends, one per action: a create carries the manifest's
// own fields and no id, since the object does not exist yet; a read,
// update, delete, tunnel, or draw carries the object's id and owner
// beside them; a list carries the kind alone. Two actions are not about
// one object: model.use carries a Key's selector and the Models it
// matches now, and usage.read carries the Key ids and the owners a query
// asks about. Every list and map is present and never null, so an
// endpoint in any language reads resource.labels as an object whether or
// not the manifest set one. ResourceFor picks the row for an action on a
// manifest object, which is what an endpoint's own tests build from:
//
//	res, ok := authorizer.ResourceFor(authorizer.ActionKeyCreate, key)
//
// An allow may carry ceilings. WireLimits is the limits object as the
// answer renders it, so an endpoint writes the type luxd decodes:
//
//	limits := authorizer.WireLimits{MaxKeySpend: &cap, MaxKeyTTL: "720h"}
//
// A member the endpoint does not set is left out, which grants nothing
// and takes nothing away; a member set to zero is sent and reads as
// zero, the same grant. DecodeLimits is the reading half, luxd's own,
// and is here so an endpoint's tests can hold their answers to it: a
// negative figure, a spend that is not a money string, or a ttl that is
// not a duration is an error, and luxd treats such an answer as no
// decision at all.
//
// A decision names a subject as the issuer and the sub joined,
// "https://login.example.com|alice", and the claims are the token's
// verbatim, which is where an endpoint reads a plan, a team, or a role
// from.
//
// The promise, as for every package at this module's root: additive
// within a module major, and the same on every build. An action string
// never changes and never disappears, a resource shape only gains
// fields, and a limits member keeps its wire name and its meaning. A new
// action is a new row in the identity spec first and a constant here
// second, so an endpoint that decides by the constants keeps compiling
// and an endpoint that decides by a default keeps deciding.
package authorizer
