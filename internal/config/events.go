// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strconv"
	"strings"
)

// The request log exporters of spec 012 and the archive's defaults.
const (
	ExporterNone    = "none"
	ExporterS3      = "s3"
	DefaultS3Region = "us-east-1"
	DefaultS3Prefix = "lux/"
)

// loadEvents reads the variables of spec 012 into c and returns every
// problem found, each naming its variable: the event sink and its
// secret, and the request log exporter with the archive's endpoint,
// region, bucket, credentials, and prefix. The secrets are never echoed.
func (c *Config) loadEvents(getenv Getenv) []string {
	var problems []string
	c.EventsURL = strings.TrimSpace(getenv("LUX_EVENTS_URL"))
	c.EventsSecret = strings.TrimSpace(getenv("LUX_EVENTS_SECRET"))
	if c.EventsURL != "" {
		u, ok := endpoint(c.EventsURL)
		switch {
		case !ok:
			problems = append(problems, "LUX_EVENTS_URL is "+strconv.Quote(c.EventsURL)+", not an http:// or https:// URL with a host")
		case !isHTTPS(u) && !isLoopback(u):
			problems = append(problems, "LUX_EVENTS_URL "+c.EventsURL+" is http:// on a host other than loopback, and the sink is reached over https:// or on loopback")
		}
		if c.EventsSecret == "" {
			problems = append(problems, "LUX_EVENTS_SECRET is unset while LUX_EVENTS_URL is set, and every delivery is signed with it")
		}
	}

	c.RequestLogExporter = withDefault(strings.TrimSpace(getenv("LUX_REQUESTLOG_EXPORTER")), ExporterNone)
	c.S3Endpoint = strings.TrimSpace(getenv("LUX_S3_ENDPOINT"))
	c.S3Region = withDefault(strings.TrimSpace(getenv("LUX_S3_REGION")), DefaultS3Region)
	c.S3Bucket = strings.TrimSpace(getenv("LUX_S3_BUCKET"))
	c.S3AccessKey = strings.TrimSpace(getenv("LUX_S3_ACCESS_KEY"))
	c.S3SecretKey = strings.TrimSpace(getenv("LUX_S3_SECRET_KEY"))
	c.S3Prefix = withDefault(strings.TrimSpace(getenv("LUX_S3_PREFIX")), DefaultS3Prefix)
	switch c.RequestLogExporter {
	case ExporterNone:
	case ExporterS3:
		for _, v := range []struct{ name, value string }{
			{"LUX_S3_ENDPOINT", c.S3Endpoint}, {"LUX_S3_BUCKET", c.S3Bucket}, {"LUX_S3_ACCESS_KEY", c.S3AccessKey}, {"LUX_S3_SECRET_KEY", c.S3SecretKey},
		} {
			if v.value == "" {
				problems = append(problems, v.name+" is unset while LUX_REQUESTLOG_EXPORTER is s3, and the archive needs it; the client reads no credential chain and has no default endpoint")
			}
		}
		if _, ok := endpoint(c.S3Endpoint); c.S3Endpoint != "" && !ok {
			problems = append(problems, "LUX_S3_ENDPOINT is "+strconv.Quote(c.S3Endpoint)+", not an http:// or https:// URL with a host")
		}
	default:
		problems = append(problems, "LUX_REQUESTLOG_EXPORTER is "+strconv.Quote(c.RequestLogExporter)+", not none or s3")
	}
	return problems
}
