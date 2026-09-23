// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package luxcli is the lux command of spec 014: the command tree, its
// flags, where the token and the Key come from, the file walk of apply,
// the four flag forms that build a manifest, the output renderers, the
// three exit codes, and the rendering of a refusal from the API's
// envelope. cmd/lux is wiring around Run; every behavior is here so a
// test drives the command without a process.
//
// The rules Run holds: output is the server's bytes unless -o asks for
// a local rendering; a request is made only once the arguments are
// understood, so exit 2 means nothing was sent and exit 1 means the
// server said no or could not be reached; a Key's value and a
// Provider's credential travel one way, out to stdout or in from the
// environment, and never reach stderr; and the command retries nothing.
package luxcli
