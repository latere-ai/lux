// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import "time"

// Defaults and bounds for the variables of spec 036. A backstop below
// five seconds re-reads the whole catalog on every replica often enough
// to show on a small store; one above ten minutes leaves an unjournaled
// status write unseen for longer than an operator reading the model list
// would expect. The grace is bounded at an hour, past which a replica
// cut off from its store serves revoked authority for longer than any
// outage the grace is meant to ride out.
const (
	DefaultCatalogReload = 30 * time.Second
	MinCatalogReload     = 5 * time.Second
	MaxCatalogReload     = 10 * time.Minute
	DefaultKeyCacheGrace = 5 * time.Minute
	MaxKeyCacheGrace     = time.Hour
)

// loadCatalog reads the variables of spec 036 into c and returns every
// problem found, each naming its variable.
func (c *Config) loadCatalog(getenv Getenv) []string {
	var problems []string
	c.CatalogReload, problems = interval("LUX_CATALOG_RELOAD", getenv("LUX_CATALOG_RELOAD"), DefaultCatalogReload, MinCatalogReload, MaxCatalogReload, problems)
	c.KeyCacheGrace, problems = interval("LUX_KEY_CACHE_GRACE", getenv("LUX_KEY_CACHE_GRACE"), DefaultKeyCacheGrace, 0, MaxKeyCacheGrace, problems)
	return problems
}
