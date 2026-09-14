// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/lux/internal/secrets"
)

// Defaults and bounds for the provider variables of spec 005. A
// discovery interval below a minute would hammer an upstream's model
// list for nothing; a health interval below five seconds would make the
// probe most of a small Provider's traffic.
const (
	DefaultDiscoveryInterval = time.Hour
	MinDiscoveryInterval     = time.Minute
	MaxDiscoveryInterval     = 24 * time.Hour
	DefaultHealthInterval    = 30 * time.Second
	MinHealthInterval        = 5 * time.Second
	MaxHealthInterval        = 10 * time.Minute
)

// loadProviders reads the variables of spec 005 into c and returns every
// problem found, each naming its variable. The key encryption keys are
// required unless LUX_MANIFEST_DIR selects the file mode, where nothing
// is sealed and the variable is read and unused.
func (c *Config) loadProviders(getenv Getenv) []string {
	var problems []string
	unset := ""
	if c.ManifestDir == "" {
		unset = "LUX_SECRETS_KEK is unset, and a credential is sealed under it in every mode but the file mode LUX_MANIFEST_DIR selects"
	}
	c.SecretsKEK, problems = parseKEK(getenv("LUX_SECRETS_KEK"), unset, problems)
	c.UpstreamAllowPrivate, problems = allowPrivate(getenv("LUX_UPSTREAM_ALLOW_PRIVATE"), problems)
	c.DiscoveryInterval, problems = interval("LUX_DISCOVERY_INTERVAL", getenv("LUX_DISCOVERY_INTERVAL"), DefaultDiscoveryInterval, MinDiscoveryInterval, MaxDiscoveryInterval, problems)
	c.HealthInterval, problems = interval("LUX_HEALTH_INTERVAL", getenv("LUX_HEALTH_INTERVAL"), DefaultHealthInterval, MinHealthInterval, MaxHealthInterval, problems)
	return problems
}

// parseKEK parses LUX_SECRETS_KEK when set, and reports the problem
// unset when the variable is blank and unset is not empty. The parser
// names a failing key by position; the variable name is prefixed here,
// and the value appears in no message.
func parseKEK(raw, unset string, problems []string) (*secrets.Keyring, []string) {
	if strings.TrimSpace(raw) == "" {
		if unset != "" {
			problems = append(problems, unset)
		}
		return nil, problems
	}
	k, err := secrets.Parse(raw)
	if err != nil {
		return nil, append(problems, "LUX_SECRETS_KEK "+err.Error())
	}
	return k, problems
}

// allowPrivate reads LUX_UPSTREAM_ALLOW_PRIVATE: 1 sets it, blank leaves
// it unset, and any other value is a problem rather than a silent no.
func allowPrivate(raw string, problems []string) (bool, []string) {
	switch strings.TrimSpace(raw) {
	case "":
		return false, problems
	case "1":
		return true, problems
	default:
		return false, append(problems, "LUX_UPSTREAM_ALLOW_PRIVATE is "+strconv.Quote(raw)+", and 1 is the one value that sets it")
	}
}

// interval reads a Go duration within bounds, or the default when blank.
func interval(name, raw string, def, minimum, maximum time.Duration, problems []string) (time.Duration, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, problems
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		return def, append(problems, name+" is "+strconv.Quote(raw)+", not a duration such as "+def.String())
	case d < minimum || d > maximum:
		return def, append(problems, name+" is "+raw+", not between "+minimum.String()+" and "+maximum.String())
	default:
		return d, problems
	}
}

// Rewrap is what the rewrap role of spec 005 reads: the keys and the
// database, and nothing else of the table.
type Rewrap struct {
	// SecretsKEK is the parsed LUX_SECRETS_KEK; the first key wraps.
	SecretsKEK *secrets.Keyring
	// DBURL is LUX_DB_URL, the one store with rows that outlive a
	// process. It is never echoed.
	DBURL string
}

// LoadRewrap reads the two variables of the rewrap role, or one error
// naming every problem found, sorted by variable name. Both are
// required: the memory store survives no process and the file mode
// seals nothing, so there is nothing durable to re-wrap without a
// database.
func LoadRewrap(getenv Getenv) (Rewrap, error) {
	var problems []string
	r := Rewrap{DBURL: strings.TrimSpace(getenv("LUX_DB_URL"))}
	r.SecretsKEK, problems = parseKEK(getenv("LUX_SECRETS_KEK"), "LUX_SECRETS_KEK is unset, and rewrap re-wraps every stored data key under its first key", problems)
	switch {
	case r.DBURL == "":
		problems = append(problems, "LUX_DB_URL is unset, and rewrap re-wraps the credential rows of a database, since the memory store survives no process and the file mode seals nothing")
	default:
		if err := checkDBURL(r.DBURL); err != nil {
			problems = append(problems, "LUX_DB_URL "+err.Error())
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Rewrap{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return r, nil
}
