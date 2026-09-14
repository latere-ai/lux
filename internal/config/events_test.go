// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"strings"
	"testing"
)

// load runs Load over a server-mode environment with the entries added.
func load(t *testing.T, extra map[string]string) (Config, error) {
	t.Helper()
	m := withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})
	maps.Copy(m, extra)
	return Load(env(m))
}

// wantProblem asserts Load refuses the environment with a problem
// carrying want, and that the secret values never appear in it.
func wantProblem(t *testing.T, extra map[string]string, want string) {
	t.Helper()
	_, err := load(t, extra)
	if err == nil {
		t.Fatalf("Load() accepted %v; want a problem containing %q", extra, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Load() = %v, want a problem containing %q", err, want)
	}
	for _, secret := range []string{extra["LUX_EVENTS_SECRET"], extra["LUX_S3_SECRET_KEY"], extra["LUX_S3_ACCESS_KEY"]} {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("Load() echoed a secret: %v", err)
		}
	}
}

// TestEventsConfigurationRefusals is spec 012's sink rows: unset is
// events off with nothing else read; the URL without the secret, an
// http:// URL off loopback, and a URL that is not one are each a
// start-up failure naming the variable; https:// and loopback http://
// load with the secret.
func TestEventsConfigurationRefusals(t *testing.T) {
	c, err := load(t, nil)
	if err != nil || c.EventsURL != "" || c.EventsSecret != "" {
		t.Fatalf("unset: %+v, %v", c, err)
	}
	c, err = load(t, map[string]string{"LUX_EVENTS_URL": "https://sink.example.com/events", "LUX_EVENTS_SECRET": " whsec-1 "})
	if err != nil || c.EventsURL != "https://sink.example.com/events" || c.EventsSecret != "whsec-1" {
		t.Fatalf("an https sink: %+v, %v", c, err)
	}
	if c, err := load(t, map[string]string{"LUX_EVENTS_URL": "http://127.0.0.1:9000/events", "LUX_EVENTS_SECRET": "whsec-canary-9f8e7d"}); err != nil || c.EventsURL == "" {
		t.Fatalf("a loopback sink: %v", err)
	}
	if c, err := load(t, map[string]string{"LUX_EVENTS_URL": "http://localhost:9000/events", "LUX_EVENTS_SECRET": "whsec-canary-9f8e7d"}); err != nil || c.EventsURL == "" {
		t.Fatalf("a localhost sink: %v", err)
	}
	if c, err := load(t, map[string]string{"LUX_EVENTS_SECRET": "orphan"}); err != nil || c.EventsSecret != "orphan" || c.EventsURL != "" {
		t.Fatalf("a secret without a URL: %+v, %v", c, err)
	}
	wantProblem(t, map[string]string{"LUX_EVENTS_URL": "https://sink.example.com/events"}, "LUX_EVENTS_SECRET is unset while LUX_EVENTS_URL is set")
	wantProblem(t, map[string]string{"LUX_EVENTS_URL": "https://sink.example.com/events", "LUX_EVENTS_SECRET": "  "}, "LUX_EVENTS_SECRET is unset while LUX_EVENTS_URL is set")
	wantProblem(t, map[string]string{"LUX_EVENTS_URL": "http://sink.example.com/events", "LUX_EVENTS_SECRET": "whsec-canary-9f8e7d"}, "LUX_EVENTS_URL http://sink.example.com/events is http:// on a host other than loopback")
	wantProblem(t, map[string]string{"LUX_EVENTS_URL": "sink.example.com", "LUX_EVENTS_SECRET": "whsec-canary-9f8e7d"}, `LUX_EVENTS_URL is "sink.example.com", not an http:// or https:// URL with a host`)
	wantProblem(t, map[string]string{"LUX_EVENTS_URL": "ftp://sink.example.com", "LUX_EVENTS_SECRET": "whsec-canary-9f8e7d"}, `LUX_EVENTS_URL is "ftp://sink.example.com", not an http:// or https:// URL with a host`)
	// Two problems from one URL join the one message.
	_, err = load(t, map[string]string{"LUX_EVENTS_URL": "http://sink.example.com/events"})
	if err == nil || strings.Count(err.Error(), "; ") != 1 || strings.Count(err.Error(), "\n") != 0 {
		t.Fatalf("want two problems in one line: %v", err)
	}
}

// TestArchiveConfigurationRefusals is spec 012's archive rows: none is
// the default and reads nothing else; s3 without the endpoint, the
// bucket, the access key, or the secret key is a start-up failure
// naming each, an endpoint that is not an absolute URL is refused, the
// region and the prefix have their defaults, and a value that is not
// none or s3 is refused.
func TestArchiveConfigurationRefusals(t *testing.T) {
	c, err := load(t, nil)
	if err != nil || c.RequestLogExporter != ExporterNone || c.S3Region != DefaultS3Region || c.S3Prefix != DefaultS3Prefix || c.S3Endpoint != "" {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	if DefaultS3Region != "us-east-1" || DefaultS3Prefix != "lux/" {
		t.Fatal("the defaults moved")
	}
	full := map[string]string{
		"LUX_REQUESTLOG_EXPORTER": "s3", "LUX_S3_ENDPOINT": "https://s3.eu-central-1.amazonaws.com", "LUX_S3_BUCKET": "lux-archive",
		"LUX_S3_ACCESS_KEY": "AKIDEXAMPLE", "LUX_S3_SECRET_KEY": "wJalrXUtnFEMI", "LUX_S3_REGION": "eu-central-1", "LUX_S3_PREFIX": "gateways/a/",
	}
	c, err = load(t, full)
	if err != nil || c.RequestLogExporter != ExporterS3 || c.S3Endpoint != full["LUX_S3_ENDPOINT"] || c.S3Bucket != "lux-archive" || c.S3AccessKey != "AKIDEXAMPLE" || c.S3SecretKey != "wJalrXUtnFEMI" || c.S3Region != "eu-central-1" || c.S3Prefix != "gateways/a/" {
		t.Fatalf("s3: %+v, %v", c, err)
	}
	minimal := map[string]string{"LUX_REQUESTLOG_EXPORTER": "s3", "LUX_S3_ENDPOINT": "http://127.0.0.1:9000", "LUX_S3_BUCKET": "b", "LUX_S3_ACCESS_KEY": "AKID-canary-1a2b", "LUX_S3_SECRET_KEY": "secret-canary-3c4d"}
	if c, err := load(t, minimal); err != nil || c.S3Region != DefaultS3Region || c.S3Prefix != DefaultS3Prefix {
		t.Fatalf("s3 with the defaults: %+v, %v", c, err)
	}
	// The four required variables are read only with s3.
	if c, err := load(t, map[string]string{"LUX_S3_BUCKET": "b"}); err != nil || c.S3Bucket != "b" {
		t.Fatalf("a bucket without the exporter: %+v, %v", c, err)
	}
	for _, name := range []string{"LUX_S3_ENDPOINT", "LUX_S3_BUCKET", "LUX_S3_ACCESS_KEY", "LUX_S3_SECRET_KEY"} {
		t.Run("without "+name, func(t *testing.T) {
			m := maps.Clone(full)
			m[name] = ""
			wantProblem(t, m, name+" is unset while LUX_REQUESTLOG_EXPORTER is s3")
		})
	}
	_, err = load(t, map[string]string{"LUX_REQUESTLOG_EXPORTER": "s3"})
	if err == nil || strings.Count(err.Error(), "is unset while LUX_REQUESTLOG_EXPORTER is s3") != 4 {
		t.Fatalf("s3 with nothing: %v", err)
	}
	bad := maps.Clone(full)
	bad["LUX_S3_ENDPOINT"] = "s3.eu-central-1.amazonaws.com"
	wantProblem(t, bad, `LUX_S3_ENDPOINT is "s3.eu-central-1.amazonaws.com", not an http:// or https:// URL with a host`)
	wantProblem(t, map[string]string{"LUX_REQUESTLOG_EXPORTER": "gcs"}, `LUX_REQUESTLOG_EXPORTER is "gcs", not none or s3`)
	wantProblem(t, map[string]string{"LUX_REQUESTLOG_EXPORTER": "S3"}, `LUX_REQUESTLOG_EXPORTER is "S3", not none or s3`)
	if c, err := load(t, map[string]string{"LUX_REQUESTLOG_EXPORTER": " none "}); err != nil || c.RequestLogExporter != ExporterNone {
		t.Fatalf("none with spaces: %+v, %v", c, err)
	}
}
