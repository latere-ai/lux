// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: ":8080", InternalAddr: ":8081"}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	c, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":   "127.0.0.1:9000",
		"LUX_INTERNAL_ADDR": "127.0.0.1:9001",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: "127.0.0.1:9000", InternalAddr: "127.0.0.1:9001"}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReportsEveryProblemInOneSortedMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":   "nope",
		"LUX_INTERNAL_ADDR": "nope",
	}))
	if err == nil {
		t.Fatal("Load() accepted two addresses that are not host:port")
	}
	got := err.Error()
	for _, want := range []string{
		"configuration: ",
		`LUX_INTERNAL_ADDR is "nope", not a host:port address`,
		`LUX_PUBLIC_ADDR is "nope", not a host:port address`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	// An address that does not parse cannot be compared as a socket, so
	// the equality problem is not reported on top of the two syntax ones.
	if strings.Contains(got, "must differ from") {
		t.Errorf("two unparseable addresses were also reported as one socket:\n%s", got)
	}
	if i, j := strings.Index(got, "LUX_INTERNAL_ADDR is"), strings.Index(got, "LUX_PUBLIC_ADDR"); i > j {
		t.Errorf("problems are not sorted by name:\n%s", got)
	}
}

func TestLoadTreatsBlankAsUnset(t *testing.T) {
	c, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "  ", "LUX_INTERNAL_ADDR": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:9000", "LUX_INTERNAL_ADDR": "127.0.0.1:9000"}))
	if err == nil || !strings.Contains(err.Error(), "must differ from LUX_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:0", "LUX_INTERNAL_ADDR": "127.0.0.1:0"})); err != nil {
		t.Fatal(err)
	}
}
