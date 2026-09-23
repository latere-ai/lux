// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package client is a typed client of the /v1 control plane API: one
// method per route, so a program that declares Providers, Models, Keys,
// and Budgets against a Lux core applies, reads, lists, deletes, and
// rotates them without assembling requests of its own. The callers are
// the lux command, a plane that drives a core it runs, and a tool that
// migrates objects into one.
//
// Every method answers with the response bytes as they arrived beside
// the status and the request id, so a caller prints or stores what the
// server sent rather than a re-encoding of it. The kinds are decoded by
// latere.ai/x/lux/manifest when a caller wants objects; this package
// decodes nothing but the error envelope and a list's members, the
// latter with each item's bytes kept. List follows next_cursor to the
// end, or to a count of items, and folds the pages into one envelope.
// The token is read per request through a TokenSource, so a file a
// helper refreshes on disk is picked up without a restart.
//
// The policy is a person's or a plane's rather than a server's: no
// retry, a ten second deadline to the first response byte and none
// after it, the process's proxy variables honored, and a refusal
// decoded from the error envelope into an *Error carrying the code, the
// fixed sentence, the paths, the developer detail, and the request id,
// apart. A caller that wants retries wraps a method; this package adds
// none, so one call is one request.
//
// The build list is the standard library and latere.ai/x/pkg/httpjson
// for the error envelope, and nothing else.
package client
