// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Two forward secrets of the least length, never echoed by a problem.
const (
	secretNew = "n-0123456789abcdef0123456789abcd"
	secretOld = "o-0123456789abcdef0123456789abcd"
)

// TestTunnelRules holds every rule of spec 013's configuration table:
// the switch takes 1 alone, the TTL is a duration between five seconds
// and five minutes, the forward address is a host:port that needs the
// tunnel on and a secret beside it, and every secret is at least 32
// bytes and named by position when it is not.
func TestTunnelRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string // a fragment of the problem, or "" for a configuration that loads
	}{
		{"the tunnel off", map[string]string{}, ""},
		{"the tunnel on", map[string]string{"LUX_TUNNEL_ENABLED": "1"}, ""},
		{"the tunnel on with a space", map[string]string{"LUX_TUNNEL_ENABLED": " 1 "}, ""},
		{"another value", map[string]string{"LUX_TUNNEL_ENABLED": "true"}, `LUX_TUNNEL_ENABLED is "true", and 1 is the one value that sets it`},
		{"a TTL", map[string]string{"LUX_TUNNEL_REGISTRY_TTL": "45s"}, ""},
		{"the TTL floor", map[string]string{"LUX_TUNNEL_REGISTRY_TTL": "5s"}, ""},
		{"the TTL ceiling", map[string]string{"LUX_TUNNEL_REGISTRY_TTL": "5m"}, ""},
		{"a TTL below the floor", map[string]string{"LUX_TUNNEL_REGISTRY_TTL": "1s"}, "LUX_TUNNEL_REGISTRY_TTL is 1s, not between 5s and 5m0s"},
		{"a TTL above the ceiling", map[string]string{"LUX_TUNNEL_REGISTRY_TTL": "10m"}, "LUX_TUNNEL_REGISTRY_TTL is 10m, not between 5s and 5m0s"},
		{"a TTL that is not a duration", map[string]string{"LUX_TUNNEL_REGISTRY_TTL": "soon"}, `LUX_TUNNEL_REGISTRY_TTL is "soon", not a duration such as 30s`},
		{"a forward", map[string]string{"LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_FORWARD_ADDR": "10.0.0.7:8081", "LUX_TUNNEL_FORWARD_SECRET": secretNew}, ""},
		{"a forward with two secrets", map[string]string{"LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_FORWARD_ADDR": "lux-1.example.com:8081", "LUX_TUNNEL_FORWARD_SECRET": secretNew + ", " + secretOld}, ""},
		{"a forward address that is not host:port", map[string]string{"LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_FORWARD_ADDR": "lux-1", "LUX_TUNNEL_FORWARD_SECRET": secretNew}, `LUX_TUNNEL_FORWARD_ADDR is "lux-1", not a host:port address`},
		{"a forward address with the tunnel off", map[string]string{"LUX_TUNNEL_FORWARD_ADDR": "10.0.0.7:8081", "LUX_TUNNEL_FORWARD_SECRET": secretNew}, "LUX_TUNNEL_FORWARD_ADDR is set while LUX_TUNNEL_ENABLED is unset"},
		{"a forward address without a secret", map[string]string{"LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_FORWARD_ADDR": "10.0.0.7:8081"}, "LUX_TUNNEL_FORWARD_SECRET is unset while LUX_TUNNEL_FORWARD_ADDR is set"},
		{"a secret without an address", map[string]string{"LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_FORWARD_SECRET": secretNew}, "LUX_TUNNEL_FORWARD_SECRET is set while LUX_TUNNEL_FORWARD_ADDR is unset"},
		{"a short secret", map[string]string{"LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_FORWARD_ADDR": "10.0.0.7:8081", "LUX_TUNNEL_FORWARD_SECRET": secretNew + ",short"}, "LUX_TUNNEL_FORWARD_SECRET entry 2 of 2 is 5 bytes, below 32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})
			maps.Copy(m, tc.env)
			c, err := Load(env(m))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Load() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, secret := range []string{secretNew, secretOld, "short"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("a problem echoed a secret: %v", err)
				}
			}
			if c.PublicURL != nil {
				t.Fatal("a refused configuration was returned")
			}
		})
	}
}

// TestTunnelValuesAreRead: each variable's value lands in its field in
// the order given, and a blank one is its default.
func TestTunnelValuesAreRead(t *testing.T) {
	c, err := Load(env(withKEK(map[string]string{
		"LUX_OIDC_ISSUERS": issuer, "LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_REGISTRY_TTL": "1m",
		"LUX_TUNNEL_FORWARD_ADDR": " 10.0.0.7:8081 ", "LUX_TUNNEL_FORWARD_SECRET": secretNew + "," + secretOld,
	})))
	if err != nil {
		t.Fatal(err)
	}
	if !c.TunnelEnabled || c.TunnelRegistryTTL != time.Minute || c.TunnelForwardAddr != "10.0.0.7:8081" || !reflect.DeepEqual(c.TunnelForwardSecrets, []string{secretNew, secretOld}) {
		t.Fatalf("values = %+v", c)
	}
	c, err = Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_TUNNEL_ENABLED": "", "LUX_TUNNEL_REGISTRY_TTL": " ", "LUX_TUNNEL_FORWARD_ADDR": "", "LUX_TUNNEL_FORWARD_SECRET": " , "})))
	if err != nil {
		t.Fatal(err)
	}
	if c.TunnelEnabled || c.TunnelRegistryTTL != DefaultTunnelRegistryTTL || c.TunnelForwardAddr != "" || c.TunnelForwardSecrets != nil {
		t.Fatalf("blank tunnel values did not fall back to defaults: %+v", c)
	}
}
