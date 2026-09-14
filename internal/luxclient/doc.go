// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package luxclient is the client of the /v1 API the lux command speaks:
// one method per route of spec 011, each returning the response bytes as
// they arrived beside the status and the request id, so the command can
// print what the server sent rather than a re-encoding of it. A list
// follows next_cursor to the end, or to a count of items, and hands back
// one envelope whose items keep their bytes. The token is read per
// request through a TokenSource, so a file a helper refreshes on disk is
// picked up without a restart.
//
// The policy is spec 014's: no retry, a ten second deadline to the first
// response byte and none after it, the process's proxy variables
// honoured, and a refusal decoded from the error envelope into an *Error
// carrying the code, the fixed sentence, the paths, the developer detail,
// and the request id, apart. The package reaches the standard library
// and latere.ai/x/pkg/httpjson for the envelope, and nothing else.
package luxclient
