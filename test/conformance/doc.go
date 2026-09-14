// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package conformance is the contract of spec 018 as executable tests:
// given a server URL and an issuer token, Run drives the manifest
// contract of spec 003, the dialect doors of spec 004, the /v1 API of
// spec 011, the Key states and limits of spec 007, the plane boundary of
// spec 006, and the usage surface of spec 009 against whatever is
// listening, one subtest per case, and reports which hold. luxd passes
// it in the integration tier; a platform that composes the packages
// behind its own front runs it against that front.
//
// TestContract is the entry point for a server this tree did not start:
// it builds Config from LUX_TEST_URL, LUX_TEST_TOKEN, LUX_TEST_SUBJECT,
// LUX_TEST_ISSUER_URL, LUX_TEST_STUBS_URL, and LUX_TEST_INTERNAL_URL,
// and skips with one line when the first is unset. A caller that starts
// its own server imports the package and calls Run with a Config whose
// Token mints through its own issuer.
//
// The suite is given the issuer token and mints nothing of that kind
// itself; the one credential it creates is its own Key, applied through
// PUT /v1/keys/{name} before the doors and keys groups and deleted at
// teardown. Every object it creates is named conf-<run>-<case> and
// carries the label conformance=<run>, and teardown deletes by that
// label and nothing else, so two runs against one server do not collide
// and the suite never touches an object it did not create. The accepted
// corpus of spec 003 is applied under the same prefix, because a suite
// that applied a Provider named openai against a serving installation
// would replace the operator's.
package conformance
