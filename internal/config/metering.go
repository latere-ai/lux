// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import "time"

// Default and bounds for LUX_METERING_FLUSH, spec 009's one variable:
// how often a replica writes its spend deltas and usage aggregates to
// the store. Below a tenth of a second the flush would be a store write
// per request in all but name; above a minute the bound on how far a
// hard limit can overshoot, (R − 1) × F × T × C + C, grows past what an
// operator would accept for the store round trips saved.
const (
	DefaultMeteringFlush = time.Second
	MinMeteringFlush     = 100 * time.Millisecond
	MaxMeteringFlush     = time.Minute
)

// loadMetering reads the variable of spec 009 into c and returns every
// problem found, each naming its variable.
func (c *Config) loadMetering(getenv Getenv) []string {
	var problems []string
	c.MeteringFlush, problems = interval("LUX_METERING_FLUSH", getenv("LUX_METERING_FLUSH"), DefaultMeteringFlush, MinMeteringFlush, MaxMeteringFlush, problems)
	return problems
}
