// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package v1 holds the types of the four kinds under the API version
// lux.latere.ai/v1beta1: Provider, Model, Key, and Budget, each with its
// metadata, spec, and status, and the scalar types the schema is built
// from, Money, Duration, Window, and the prefixed ULID of NewID. It imports
// the standard library alone, so a package that needs the shape of an
// object and nothing of the resolver, metering for Pricing and Money,
// imports it without pulling in the YAML decoder.
//
// The two write-only fields, Provider.spec.credential.value and
// Key.spec.value, are unexported members reached through accessors, so no
// encoder, JSON or YAML, can serialize a value by reflection. Spec 003 is
// the contract; this package is its Go shape.
package v1
