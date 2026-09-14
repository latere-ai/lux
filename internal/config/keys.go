// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strconv"
	"strings"
	"time"
)

// Defaults and bounds for the key variables of spec 007. A cache below a
// second would put the store back on the hot path; one above ten
// minutes would let a revoked Key serve for longer than an operator
// would accept on a replica whose journal tail has stalled.
const (
	DefaultKeyCache = 10 * time.Second
	MinKeyCache     = time.Second
	MaxKeyCache     = 10 * time.Minute
)

// loadKeys reads the variables of spec 007 into c and returns every
// problem found, each naming its variable.
func (c *Config) loadKeys(getenv Getenv) []string {
	var problems []string
	c.KeyCache, problems = interval("LUX_KEY_CACHE", getenv("LUX_KEY_CACHE"), DefaultKeyCache, MinKeyCache, MaxKeyCache, problems)
	c.DefaultRequestsPerMinute, problems = count("LUX_DEFAULT_REQUESTS_PER_MINUTE", getenv("LUX_DEFAULT_REQUESTS_PER_MINUTE"), problems)
	c.DefaultTokensPerMinute, problems = count("LUX_DEFAULT_TOKENS_PER_MINUTE", getenv("LUX_DEFAULT_TOKENS_PER_MINUTE"), problems)
	return problems
}

// count reads a whole number of zero or more, or zero when blank.
func count(name, raw string, problems []string) (int, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, problems
	}
	n, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		return 0, append(problems, name+" is "+strconv.Quote(raw)+", not a whole number")
	case n < 0:
		return 0, append(problems, name+" is "+raw+", below 0, and 0 is no limit")
	default:
		return n, problems
	}
}
