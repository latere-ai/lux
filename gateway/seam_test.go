// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

// The seam between spec 004's handler and spec 005's upstream client:
// the concrete Clients that NewClientSource builds is what Options.Clients
// takes, so the two land in one package without a duplicate symbol and
// the handler dials through nothing else.
var _ ClientSource = (*Clients)(nil)
