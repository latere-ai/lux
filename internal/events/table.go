// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

// Row is one line of spec 012's type table as the writers emit it: the
// reason its source writes, the members data always carries, and the
// members present when the fact they report is.
type Row struct {
	Reason   string
	Members  []string
	Optional []string
}

// Table is the type table, one row per type, which a sink author and
// TestEventTable read. A type not in it does not exist.
var Table = map[string]Row{
	ProviderCreated:     {Reason: ReasonRequest, Members: []string{"dialect", "baseURL"}},
	ProviderUpdated:     {Reason: ReasonRequest, Members: []string{"paths"}},
	ProviderDeleted:     {Reason: ReasonRequest},
	ProviderUnreachable: {Reason: ReasonProbe, Members: []string{"since", "lastError", "targets"}},
	ProviderHealthy:     {Reason: ReasonProbe, Members: []string{"since"}, Optional: []string{"wasUnreachableFor"}},
	ModelCreated:        {Reason: ReasonRequest, Members: []string{"targets", "priced"}},
	ModelUpdated:        {Reason: ReasonRequest, Members: []string{"paths"}},
	ModelDeleted:        {Reason: ReasonRequest},
	ModelDiscovered:     {Reason: ReasonDiscovery, Members: []string{"provider", "upstreamModel"}},
	ModelRemoved:        {Reason: ReasonDiscovery, Members: []string{"provider", "upstreamModel"}},
	KeyCreated:          {Reason: ReasonRequest, Members: []string{"prefix", "models", "budget"}, Optional: []string{"expiresAt"}},
	KeyUpdated:          {Reason: ReasonRequest, Members: []string{"paths"}},
	KeyFenced:           {Reason: ReasonRequest},
	KeyRotated:          {Reason: ReasonRequest, Members: []string{"prefix", "previousPrefix"}},
	KeyDeleted:          {Reason: ReasonRequest, Members: []string{"prefix"}},
	KeyExhausted:        {Reason: ReasonLimit, Members: []string{"window", "amount", "spent", "currency"}, Optional: []string{"resetsAt"}},
	BudgetCreated:       {Reason: ReasonRequest, Members: []string{"currency", "window", "hard"}, Optional: []string{"amount"}},
	BudgetUpdated:       {Reason: ReasonRequest, Members: []string{"paths"}},
	BudgetDeleted:       {Reason: ReasonRequest},
	BudgetExhausted:     {Reason: ReasonLimit, Members: []string{"window", "amount", "spent", "currency", "hard"}, Optional: []string{"resetsAt"}},
	CheckPing:           {Reason: ReasonCheck},
}
