// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package metering is the arithmetic of usage. Spec 007 places here the
// windows a spend limit and a Budget count in, the key that names one
// window of one object in the store's counter table, the token
// reservation a request charges before it runs and the settle after,
// the bound on how far several replicas may overshoot a spend limit
// before they agree, the marker one replica claims to announce an
// exhausted window once, and one replica's unflushed deltas over the
// store's totals. Spec 009 adds the record of one request built from
// the gateway's with the cost a Model's pricing gives its tokens, the
// hourly aggregate rows the store keeps and the response rows the usage
// API answers, the pure fold from records to either, and the query with
// its bounds and the intersection with an authorizer's filter.
//
// The package computes and dials nothing: it imports manifest/v1 and
// the standard library, takes the counter table through CounterStore,
// and is driven by the gateway's Limiter on one side and the API on the
// other. A window computed by one build is computed the same by every
// later build for the same clock, which is what lets replicas agree on
// a boundary without being told.
package metering
