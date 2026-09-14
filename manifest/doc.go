// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package manifest is the contract of spec 003 a caller codes against:
// Decode reads a manifest of any of the four kinds from YAML or JSON with
// one decoder and refuses an unknown field with its path; Resolve runs
// the six stages that turn a decoded object into the one the gateway acts
// on, structural validation, defaulting, the references through a Lookup,
// the authorizer's ceilings, the update rules against the existing object,
// and the cross-field consistency; Match is the glob the gateway matches a
// request's model against a Key's selectors with; Corpus is the golden
// corpus the conformance suite reads through the import.
//
// The package imports manifest/v1, github.com/goccy/go-yaml for the YAML
// parse, and the standard library, and nothing under internal/, so the
// API, the file mode, the lux command, and a platform that wants the
// contract without the server all mean the same thing by a manifest.
package manifest
