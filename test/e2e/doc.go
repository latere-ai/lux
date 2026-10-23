// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package e2e is the integration tier: luxd and lux-stubs as processes,
// driven over HTTP. Every test file here carries the integration build tag
// and compiles only when it is set.
//
// This file carries no tag on purpose. Without one untagged file the
// package has no Go files in an ordinary build, and golangci-lint's
// typecheck then reports "build constraints exclude all Go files" and
// exits non-zero, which the pre-push hook reads as a lint failure for any
// push that touches the directory. One untagged declaration gives the
// linter a package to see and changes nothing about which tests run.
package e2e
