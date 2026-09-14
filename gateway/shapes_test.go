// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import "latere.ai/x/pkg/llmdialect/bridge"

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

// openaiError is the OpenAI error shape.
type openaiError struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    string  `json:"code"`
		Param   *string `json:"param"`
	} `json:"error"`
}

// anthropicError is the Anthropic error shape.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// geminiError is Google's error shape with the lux code in
// details[0].reason.
type geminiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
			Domain string `json:"domain"`
		} `json:"details"`
	} `json:"error"`
}

// googleStatus is the google.rpc.Code name the Gemini shape carries for
// an HTTP status, which the bridge owns.
func googleStatus(status int) string { return bridge.GoogleStatus(status) }
