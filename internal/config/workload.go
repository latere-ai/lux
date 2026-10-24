// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net/url"
	"strconv"
	"strings"
)

// DefaultWorkloadAudience is the audience a token minted for the pod's
// service account names when LUX_WORKLOAD_IDENTITY_AUDIENCE is unset:
// Anthropic's, the one upstream that exchanges one (spec 051).
const DefaultWorkloadAudience = "https://api.anthropic.com"

// WorkloadIdentity is where the gateway's own identity token comes from,
// which a federated Provider's credential is exchanged for (spec 051).
// With neither source the gateway has none.
type WorkloadIdentity struct {
	// File is LUX_WORKLOAD_IDENTITY_FILE: a path holding a JWT, read
	// again for every exchange.
	File string
	// Kubernetes is LUX_WORKLOAD_IDENTITY_KUBERNETES: mint a token for the
	// pod's own service account for every exchange.
	Kubernetes bool
	// Audience is LUX_WORKLOAD_IDENTITY_AUDIENCE, the audience of a
	// minted token; the file's token carries its own.
	Audience string
}

// Enabled reports whether the gateway has a workload identity.
func (w WorkloadIdentity) Enabled() bool { return w.File != "" || w.Kubernetes }

// loadWorkloadIdentity reads the variables of spec 051 into c and
// returns every problem found, each naming its variable: the two sources
// are exclusive, and the audience is an absolute URL or a plain name.
func (c *Config) loadWorkloadIdentity(getenv Getenv) []string {
	var problems []string
	w := &c.WorkloadIdentity
	w.File = strings.TrimSpace(getenv("LUX_WORKLOAD_IDENTITY_FILE"))
	w.Kubernetes, problems = flag("LUX_WORKLOAD_IDENTITY_KUBERNETES", getenv("LUX_WORKLOAD_IDENTITY_KUBERNETES"), problems)
	if w.File != "" && w.Kubernetes {
		problems = append(problems, "LUX_WORKLOAD_IDENTITY_FILE and LUX_WORKLOAD_IDENTITY_KUBERNETES are both set, and the identity token comes from one of them")
	}
	w.Audience = strings.TrimSpace(getenv("LUX_WORKLOAD_IDENTITY_AUDIENCE"))
	switch {
	case w.Audience == "":
		w.Audience = DefaultWorkloadAudience
	case strings.ContainsAny(w.Audience, " ,\t"):
		problems = append(problems, "LUX_WORKLOAD_IDENTITY_AUDIENCE is "+strconv.Quote(w.Audience)+", and it names one audience")
	case strings.Contains(w.Audience, "://"):
		if u, err := url.Parse(w.Audience); err != nil || u.Host == "" {
			problems = append(problems, "LUX_WORKLOAD_IDENTITY_AUDIENCE is "+strconv.Quote(w.Audience)+", not an absolute URL")
		}
	}
	return problems
}
