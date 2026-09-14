// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package config reads the typed configuration of luxd from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	DefaultDBMaxConns   = 8
)

// The bounds of LUX_DB_MAX_CONNS. A managed cluster caps its connections
// in the low tens and a replica set multiplies whatever one process
// opens, so the ceiling is what one process could defensibly hold.
const (
	MinDBMaxConns = 1
	MaxDBMaxConns = 100
)

// Getenv is the environment lookup Load reads through, so a test passes a
// map and never touches the process environment.
type Getenv func(string) string

// Config is the resolved configuration. Field names follow the variable
// names in spec 002 without the LUX_ prefix.
type Config struct {
	// PublicAddr is where the /v1 API, the model traffic, and the public
	// probes listen.
	PublicAddr string
	// InternalAddr is where the four probes listen for the cluster.
	InternalAddr string
	// ManifestDir is the directory of manifests that is desired state in
	// the file mode of spec 010; empty is not the file mode.
	ManifestDir string
	// DBURL is the postgres:// URL of the Postgres store of spec 010;
	// empty is the memory store. It is never echoed, because it may carry
	// a password.
	DBURL string
	// DBMaxConns is the pool size with DBURL, DefaultDBMaxConns without.
	DBMaxConns int
}

// Load reads every variable through getenv and returns the configuration,
// or one error naming every problem found, sorted by variable name.
func Load(getenv Getenv) (Config, error) {
	c := Config{
		PublicAddr:   withDefault(getenv("LUX_PUBLIC_ADDR"), DefaultPublicAddr),
		InternalAddr: withDefault(getenv("LUX_INTERNAL_ADDR"), DefaultInternalAddr),
		ManifestDir:  strings.TrimSpace(getenv("LUX_MANIFEST_DIR")),
		DBURL:        strings.TrimSpace(getenv("LUX_DB_URL")),
		DBMaxConns:   DefaultDBMaxConns,
	}
	var problems []string
	if err := checkAddr(c.PublicAddr); err != nil {
		problems = append(problems, "LUX_PUBLIC_ADDR "+err.Error())
	}
	if err := checkAddr(c.InternalAddr); err != nil {
		problems = append(problems, "LUX_INTERNAL_ADDR "+err.Error())
	}
	if sameEndpoint(c.PublicAddr, c.InternalAddr) {
		problems = append(problems, "LUX_INTERNAL_ADDR must differ from LUX_PUBLIC_ADDR; both are "+c.PublicAddr)
	}
	if c.ManifestDir != "" {
		if err := checkDir(c.ManifestDir); err != nil {
			problems = append(problems, "LUX_MANIFEST_DIR "+err.Error())
		}
	}
	if c.DBURL != "" {
		if err := checkDBURL(c.DBURL); err != nil {
			problems = append(problems, "LUX_DB_URL "+err.Error())
		}
		// The pool size is read only with a database to size it for.
		if raw := strings.TrimSpace(getenv("LUX_DB_MAX_CONNS")); raw != "" {
			n, err := strconv.Atoi(raw)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("LUX_DB_MAX_CONNS is %q, not an integer", raw))
			case n < MinDBMaxConns || n > MaxDBMaxConns:
				problems = append(problems, fmt.Sprintf("LUX_DB_MAX_CONNS is %d, not between %d and %d", n, MinDBMaxConns, MaxDBMaxConns))
			default:
				c.DBMaxConns = n
			}
		}
	}
	// The two answer the same question, where desired state comes from,
	// and a precedence rule between them would be a silent decision
	// about whose manifests win.
	if c.ManifestDir != "" && c.DBURL != "" {
		problems = append(problems, "LUX_DB_URL and LUX_MANIFEST_DIR are both set; desired state comes from the directory or the database, not both")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Config{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

func withDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// sameEndpoint reports whether two valid addresses name one socket. Port
// 0 asks the kernel for any free port, so two ":0" addresses are two
// sockets and a test that binds both on loopback is not refused.
func sameEndpoint(a, b string) bool {
	if a != b {
		return false
	}
	_, port, err := net.SplitHostPort(a)
	return err == nil && port != "0"
}

// checkAddr accepts what net.Listen accepts for a TCP address: host:port
// with the host optional.
func checkAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("is %q, not a host:port address", addr)
	}
	return nil
}

// checkDir accepts a directory this process can list.
func checkDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("is %q, not a readable directory: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("is %q, not a directory", dir)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return fmt.Errorf("is %q, not a readable directory: %w", dir, err)
	}
	return nil
}

// checkDBURL accepts a postgres:// or postgresql:// URL, the two
// spellings the driver takes, and echoes neither the value nor the
// parser's error, both of which would carry the password.
func checkDBURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("does not parse as a URL; the value is not echoed because it may carry a password")
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		return nil
	default:
		return fmt.Errorf("has scheme %q, not postgres", u.Scheme)
	}
}
