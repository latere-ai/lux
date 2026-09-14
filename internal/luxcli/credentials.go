// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"strings"

	"latere.ai/x/lux/internal/luxclient"
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

// controlClient is the client of a /v1 command: LUX_URL and a token from
// LUX_TOKEN or LUX_TOKEN_FILE, one of them. A Key alone is a usage
// error naming the boundary, since the gateway's answer would be about
// the server and not about the mistake.
func (a *app) controlClient() (*luxclient.Client, error) {
	url := first(a.url, a.o.Getenv(EnvURL))
	if url == "" {
		return nil, &usageError{msg: "Set " + EnvURL + " to the gateway's URL."}
	}
	token, tokenFile := a.token, a.tokenFile
	if token == "" && tokenFile == "" {
		token, tokenFile = a.o.Getenv(EnvToken), a.o.Getenv(EnvTokenFile)
	}
	switch {
	case token != "" && tokenFile != "":
		return nil, &usageError{msg: "Set one of " + EnvToken + " and " + EnvTokenFile + ", not both."}
	case token == "" && tokenFile == "" && first(a.key, a.o.Getenv(EnvKey)) != "":
		return nil, &usageError{msg: EnvKey + " opens a door, not /v1. Set " + EnvToken + " or " + EnvTokenFile + " to a token from your issuer."}
	case token == "" && tokenFile == "":
		return nil, &usageError{msg: "Set " + EnvToken + " or " + EnvTokenFile + " to a token from your issuer."}
	}
	c := a.client(url)
	if tokenFile != "" {
		c.Token = luxclient.FileToken(tokenFile)
	} else {
		c.Token = luxclient.StaticToken(token)
	}
	return c, nil
}

// doorClient is the client of a door command: LUX_URL then LUX_BASE_URL,
// and a Key from LUX_KEY then LUX_API_KEY. A token alone is the mirror
// of controlClient's usage error.
func (a *app) doorClient() (*luxclient.Client, error) {
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
	c.Token = luxclient.StaticToken(key)
	return c, nil
}

// bareClient reaches the documents no bearer guards.
func (a *app) bareClient(url string) *luxclient.Client { return a.client(url) }

func (a *app) client(url string) *luxclient.Client {
	return &luxclient.Client{BaseURL: strings.TrimRight(url, "/"), HTTP: a.o.HTTP, UserAgent: "lux/" + a.o.Version}
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
