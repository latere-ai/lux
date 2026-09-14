// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package auth is the control plane's identity of spec 006: who is
// applying this manifest, and may they. A Verifier accepts a bearer from
// any issuer the operator lists, through latere.ai/x/pkg/authkit/jwt, and
// renders the caller as a subject with every claim of the token verbatim.
// An Authorizer asks one endpoint the operator writes, through the shared
// contract of latere.ai/x/pkg/authz, in the action vocabulary of
// latere.ai/x/lux/authorizer with the resource shape of each action, and
// reads the answer into Lux's codes; a Lookup carries the same questions
// into manifest.Resolve; an OwnerPolicy is the decision an installation
// makes when no authorizer is configured. Startup builds all of it from
// the configuration and refuses to start on an issuer it cannot read.
//
// The package reads no claim for meaning and decides nothing on the data
// plane: a Key is spec 007's and a door asks nothing here.
package auth
