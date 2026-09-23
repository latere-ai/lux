// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestTunnelProviderSchema is spec 013's manifest row in one table: a
// Provider with tunnel: true resolves without baseURL and without
// credential, with no credential block defaulted in; either one present
// is exclusive_fields naming both paths; tunnel changed on update, in
// either direction, is immutable_field at spec.tunnel; and tunnel: true
// with the tunnel off, in server mode or in the file mode, is
// invalid_field at spec.tunnel with LUX_TUNNEL_ENABLED in the detail.
func TestTunnelProviderSchema(t *testing.T) {
	on := corpusOptions(t)
	off := on
	off.TunnelEnabled = false
	file := off
	file.FileMode = true
	tunneled := "spec:\n  dialect: openai\n  tunnel: true\n"
	dialed := minProvider
	existing := func(tunnel bool) v1.Object {
		body := head(v1.KindProvider, "laptop") + dialed
		if tunnel {
			body = head(v1.KindProvider, "laptop") + tunneled
		}
		r := mustResolve(t, body, on)
		p := r.Object.(*v1.Provider)
		p.Status.ID = "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y"
		return p
	}
	for _, tc := range []struct {
		name     string
		body     string
		o        Options
		existing v1.Object
		code     Code
		paths    []string
		detail   string
	}{
		{"tunnel with a baseURL", tunneled + "  baseURL: https://api.example.com/v1\n", on, nil, CodeExclusiveFields, []string{"spec.tunnel", "spec.baseURL"}, "no address"},
		{"tunnel with a credential value", tunneled + "  credential: {value: sk-x}\n", on, nil, CodeExclusiveFields, []string{"spec.tunnel", "spec.credential"}, "the tunnel is the credential"},
		{"tunnel with a credential from the environment", tunneled + "  credential: {valueFrom: {env: KEY}}\n", file, nil, CodeExclusiveFields, []string{"spec.tunnel", "spec.credential"}, "the tunnel is the credential"},
		{"tunnel turned on by an update", tunneled, on, existing(false), CodeImmutableField, []string{"spec.tunnel"}, ""},
		{"tunnel turned off by an update", dialed, on, existing(true), CodeImmutableField, []string{"spec.tunnel"}, ""},
		{"tunnel with the tunnel off", tunneled, off, nil, CodeInvalidField, []string{"spec.tunnel"}, "LUX_TUNNEL_ENABLED"},
		{"tunnel in the file mode", tunneled, file, nil, CodeInvalidField, []string{"spec.tunnel"}, "LUX_TUNNEL_ENABLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.o
			o.Existing = tc.existing
			e := resolveErr(t, head(v1.KindProvider, "laptop")+tc.body, o)
			if e.Code != tc.code || strings.Join(e.Paths, ",") != strings.Join(tc.paths, ",") || !strings.Contains(e.Detail, tc.detail) {
				t.Fatalf("got %s at %v (%s), want %s at %v with %q", e.Code, e.Paths, e.Detail, tc.code, tc.paths, tc.detail)
			}
		})
	}
	// The accepted shape: no baseURL, no credential block at all, the
	// discovery and health defaults, and the field visible as true.
	r := mustResolve(t, head(v1.KindProvider, "laptop")+tunneled, on)
	p := r.Object.(*v1.Provider)
	if !p.Spec.Tunnel || p.Spec.BaseURL != "" || p.Spec.Credential != nil || p.Spec.Discovery.Mode != v1.DiscoveryAuto || p.Spec.Health.Mode != v1.HealthProbe {
		t.Fatalf("resolved %+v", p.Spec)
	}
	// An update that keeps tunnel true is accepted, with the field kept.
	o := on
	o.Existing = existing(true)
	if _, err := Resolve(context.Background(), mustDecode(t, head(v1.KindProvider, "laptop")+tunneled+"  timeout: 30s\n"), o); err != nil {
		t.Fatalf("an update keeping tunnel: %v", err)
	}
}
