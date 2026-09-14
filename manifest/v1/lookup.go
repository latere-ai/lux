// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// ModelRef is one matched Model as the authorizer sees it in the model.use
// resource of spec 006: its id, its name, and its owner. It lives here
// beside the kinds so a Lookup implementation imports the types alone.
type ModelRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
}
