// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"regexp"
	"time"

	"latere.ai/x/lux/metering"
)

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
	rules, err := metering.ParseRetentionRules(getenv("LUX_USAGE_RETENTION"))
	if err != nil {
		problems = append(problems, "LUX_USAGE_RETENTION: "+err.Error())
	}
	c.UsageRetention = rules
	c.RequestLogPartitionLabel = getenv("LUX_REQUESTLOG_PARTITION_LABEL")
	if c.RequestLogPartitionLabel != "" && !partitionLabel.MatchString(c.RequestLogPartitionLabel) {
		problems = append(problems, "LUX_REQUESTLOG_PARTITION_LABEL is "+c.RequestLogPartitionLabel+", not a label name: [A-Za-z0-9._/-] of at most 253 characters beginning and ending alphanumeric")
	}
	return problems
}

// partitionLabel is the label name grammar of a manifest's labels, an
// optional DNS prefix and a slash before the name.
var partitionLabel = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_./]{0,251}[A-Za-z0-9])?$`)
