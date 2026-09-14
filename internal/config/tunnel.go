// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strconv"
	"strings"
	"time"
)

// Defaults and bounds for the tunnel variables of spec 013. A registry
// row lives one liveness window past its last heartbeat, so a window
// below five seconds would make a laptop's every network hiccup an
// Unreachable Provider, and one above five minutes would keep a dead
// tunnel selectable for longer than a caller waits. A forward secret is
// an installation-internal bearer compared in constant time; below 32
// bytes it is guessable from inside the cluster network it guards
// against.
const (
	DefaultTunnelRegistryTTL = 30 * time.Second
	MinTunnelRegistryTTL     = 5 * time.Second
	MaxTunnelRegistryTTL     = 5 * time.Minute
	MinTunnelSecretBytes     = 32
)

// loadTunnel reads the variables of spec 013 into c and returns every
// problem found, each naming its variable and never a secret's value.
// The forward address and the secret come together: the route the
// address advertises is what the secret locks, so one without the
// other is a problem, and both without the tunnel on is a forward for
// a route that is not served.
func (c *Config) loadTunnel(getenv Getenv) []string {
	var problems []string
	c.TunnelEnabled, problems = flag("LUX_TUNNEL_ENABLED", getenv("LUX_TUNNEL_ENABLED"), problems)
	c.TunnelRegistryTTL, problems = interval("LUX_TUNNEL_REGISTRY_TTL", getenv("LUX_TUNNEL_REGISTRY_TTL"), DefaultTunnelRegistryTTL, MinTunnelRegistryTTL, MaxTunnelRegistryTTL, problems)
	c.TunnelForwardAddr = strings.TrimSpace(getenv("LUX_TUNNEL_FORWARD_ADDR"))
	if c.TunnelForwardAddr != "" {
		if err := checkAddr(c.TunnelForwardAddr); err != nil {
			problems = append(problems, "LUX_TUNNEL_FORWARD_ADDR "+err.Error())
		}
		if !c.TunnelEnabled {
			problems = append(problems, "LUX_TUNNEL_FORWARD_ADDR is set while LUX_TUNNEL_ENABLED is unset, and the forward route serves the tunnel")
		}
	}
	c.TunnelForwardSecrets, problems = bearers("LUX_TUNNEL_FORWARD_SECRET", getenv("LUX_TUNNEL_FORWARD_SECRET"), problems)
	switch {
	case c.TunnelForwardAddr != "" && len(c.TunnelForwardSecrets) == 0:
		problems = append(problems, "LUX_TUNNEL_FORWARD_SECRET is unset while LUX_TUNNEL_FORWARD_ADDR is set, and the forward route accepts nothing without it")
	case c.TunnelForwardAddr == "" && len(c.TunnelForwardSecrets) > 0:
		problems = append(problems, "LUX_TUNNEL_FORWARD_SECRET is set while LUX_TUNNEL_FORWARD_ADDR is unset, and the secret locks the route the address advertises")
	}
	return problems
}

// flag reads a variable that is set by the one value 1: blank leaves it
// unset, and any other value is a problem rather than a silent no.
func flag(name, raw string, problems []string) (bool, []string) {
	switch strings.TrimSpace(raw) {
	case "":
		return false, problems
	case "1":
		return true, problems
	default:
		return false, append(problems, name+" is "+strconv.Quote(raw)+", and 1 is the one value that sets it")
	}
}

// bearers reads a comma separated list of secrets, each at least
// MinTunnelSecretBytes, in the order given. An entry that is too short
// is named by position and never by value.
func bearers(name, raw string, problems []string) ([]string, []string) {
	var out []string
	entries := splitList(raw)
	for i, s := range entries {
		if len(s) < MinTunnelSecretBytes {
			problems = append(problems, name+" entry "+strconv.Itoa(i+1)+" of "+strconv.Itoa(len(entries))+" is "+strconv.Itoa(len(s))+" bytes, below "+strconv.Itoa(MinTunnelSecretBytes))
			continue
		}
		out = append(out, s)
	}
	return out, problems
}
