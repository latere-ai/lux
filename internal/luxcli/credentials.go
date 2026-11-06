// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"strings"

	"latere.ai/x/lux/client"
)

// plane is which credential a command carries: an issuer token to /v1,
// a Key to a door, or nothing for the two documents no bearer guards.
// planeUsage is the control plane for the two commands whose -key flag
// is a filter rather than the credential.
type plane int

const (
	planeControl plane = iota
	planeUsage
	planeDoor
	planeNone
)

// bearerSource is where a /v1 command's bearer comes from: the flags
// first and the environment after them, one of the two and never both.
// Neither is a nil source: the command sends no Authorization header
// and the server answers, which a control plane in the file mode does,
// since it takes no bearer. A Key is never the bearer here, so a shell
// that exported one for the doors reads that control plane unchanged;
// the refusal of a server that wants a token names the boundary
// instead (noTokenLine). refreshable reports a token file, whose bytes
// a long lux serve reads again for every request and every heartbeat.
func (a *app) bearerSource() (source client.TokenSource, refreshable bool, err error) {
	token, tokenFile := a.token, a.tokenFile
	if token == "" && tokenFile == "" {
		token, tokenFile = a.o.Getenv(EnvToken), a.o.Getenv(EnvTokenFile)
	}
	switch {
	case token != "" && tokenFile != "":
		return nil, false, &usageError{msg: "Set one of " + EnvToken + " and " + EnvTokenFile + ", not both."}
	case token == "" && tokenFile == "":
		return nil, false, nil
	case tokenFile != "":
		return client.FileToken(tokenFile), true, nil
	}
	return client.StaticToken(token), false, nil
}

// noTokenLine is what a /v1 command adds to an unauthenticated refusal
// when it sent no bearer, and the usage error of lux serve, which always
// needs one: the two token variables, after the boundary when a Key is
// what the shell holds.
func (a *app) noTokenLine() string {
	line := "Set " + EnvToken + " or " + EnvTokenFile + " to a token from your issuer."
	if first(a.key, a.o.Getenv(EnvKey)) != "" {
		return EnvKey + " opens a door, not /v1. " + line
	}
	return line
}

// controlClient is the client of a /v1 command: LUX_URL and the bearer
// of bearerSource, or none.
func (a *app) controlClient() (*client.Client, error) {
	url := first(a.url, a.o.Getenv(EnvURL))
	if url == "" {
		return nil, &usageError{msg: "Set " + EnvURL + " to the gateway's URL."}
	}
	source, _, err := a.bearerSource()
	if err != nil {
		return nil, err
	}
	a.bearerless = source == nil
	c := a.client(url)
	c.Token = source
	// A LUX_URL with a path may name an installation whose base path
	// stands in the place of /v1 (spec 040); its /.well-known/lux says
	// which. A LUX_URL without a path sends nothing.
	if err := c.Discover(a.ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// doorClient is the client of a door command: LUX_URL then LUX_BASE_URL,
// and a Key from LUX_KEY then LUX_API_KEY. A token alone is the mirror
// of controlClient's usage error.
func (a *app) doorClient() (*client.Client, error) {
	url := first(a.url, a.o.Getenv(EnvURL), a.o.Getenv(EnvSDKURL))
	if url == "" {
		return nil, &usageError{msg: "Set " + EnvURL + " to the gateway's URL."}
	}
	key := first(a.key, a.o.Getenv(EnvKey), a.o.Getenv(EnvSDKKey))
	if key == "" {
		if first(a.token, a.tokenFile, a.o.Getenv(EnvToken), a.o.Getenv(EnvTokenFile)) != "" {
			return nil, &usageError{msg: EnvToken + " opens /v1, not a door. Set " + EnvKey + " to a Key value."}
		}
		return nil, &usageError{msg: "Set " + EnvKey + " to a Key value."}
	}
	c := a.client(url)
	c.Token = client.StaticToken(key)
	return c, nil
}

// bareClient reaches the documents no bearer guards.
func (a *app) bareClient(url string) *client.Client { return a.client(url) }

func (a *app) client(url string) *client.Client {
	return &client.Client{BaseURL: strings.TrimRight(url, "/"), HTTP: a.o.HTTP, UserAgent: "lux/" + a.o.Version}
}

// first is the first non-empty string.
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
