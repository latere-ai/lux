// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"net/url"
	"sort"
	"strings"

	"latere.ai/x/lux/internal/localissuer"
)

// Token is what the token role of spec 035 reads: the local issuer's
// name, its signing key, the audiences a token may be minted for, and the
// admin subjects the default subject is taken from. Nothing else of the
// table: the role opens no store and seals nothing, so LUX_SECRETS_KEK
// and the database variables are neither read nor required, and luxd
// token runs wherever the key is, beside a serving installation or not.
type Token struct {
	// PublicURL is LUX_PUBLIC_URL, the local issuer's name and the iss
	// of every token minted.
	PublicURL *url.URL
	// LocalIssuerKey is the parsed LUX_LOCAL_ISSUER_KEY, nil when unset,
	// which the role answers as a usage error naming the variable rather
	// than a configuration problem.
	LocalIssuerKey *localissuer.Key
	// OIDCAudiences is LUX_OIDC_AUDIENCE, the primary first.
	OIDCAudiences []string
	// AdminSubjects is LUX_ADMIN_SUBJECTS.
	AdminSubjects []string
}

// LoadToken reads the four variables of the token role through the
// parsers Load uses, so a problem reads the same in both, or returns one
// error naming every problem found, sorted by variable name.
func LoadToken(getenv Getenv) (Token, error) {
	var problems []string
	var t Token
	t.PublicURL, problems = publicURL(getenv("LUX_PUBLIC_URL"), problems)
	t.LocalIssuerKey, problems = localIssuerKey(getenv("LUX_LOCAL_ISSUER_KEY"), problems)
	t.OIDCAudiences = audiences(getenv("LUX_OIDC_AUDIENCE"), &problems)
	t.AdminSubjects = adminSubjects(getenv("LUX_ADMIN_SUBJECTS"), &problems)
	if len(problems) > 0 {
		sort.Strings(problems)
		return Token{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return t, nil
}
