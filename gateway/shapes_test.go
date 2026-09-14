// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

// The decode shapes the door-level tests read a body back through. The
// bridge writes these shapes and this package declares none of them;
// the tests keep reading a body member by member rather than through
// the bridge's own parser, so an assertion here stays independent of
// the writer it checks. The bytes themselves are held by the goldens.

type openaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type openaiModelList struct {
	Object string        `json:"object"`
	Data   []openaiModel `json:"data"`
}
