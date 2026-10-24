// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// federated is an anthropic Provider's spec with a federated credential.
const federated = "spec:\n  dialect: anthropic\n  baseURL: https://api.example.com/v1\n  credential:\n    federation:\n" +
	"      ruleID: fdrl_01ExampleRule\n      organizationID: 4f6c1d2a-8e3b-4c7d-9a1e-2b5f6c7d8e9f\n      serviceAccountID: svac_01ExampleServiceAccount\n"

// TestFederationNeedsWorkloadIdentity is spec 051's first criterion past
// the corpus: a federation beside a variable or an empty value is
// exclusive_fields; a federated Provider is refused where the gateway has no
// workload identity, in server mode and in the file mode, naming the two
// variables; its credential header is Authorization, so a static header
// of that name is refused; and an explicit Authorization, bearer is
// accepted as the default is.
func TestFederationNeedsWorkloadIdentity(t *testing.T) {
	on := corpusOptions(t)
	off := on
	off.WorkloadIdentity = false
	file := off
	file.FileMode = true
	for _, o := range []Options{off, file} {
		e := resolveErr(t, head(v1.KindProvider, "anthropic")+federated, o)
		if e.Code != CodeInvalidField || strings.Join(e.Paths, ",") != "spec.credential.federation" ||
			!strings.Contains(e.Detail, "LUX_WORKLOAD_IDENTITY_FILE") || !strings.Contains(e.Detail, "LUX_WORKLOAD_IDENTITY_KUBERNETES") {
			t.Errorf("file mode %v: %s %v %q", o.FileMode, e.Code, e.Paths, e.Detail)
		}
	}
	// Beside a variable in server mode, or an empty value, the refusal is
	// the federation's own, not the value's.
	for name, extra := range map[string]string{"valueFrom": "    valueFrom:\n      env: KEY\n", "an empty value": "    value: \"\"\n"} {
		body := head(v1.KindProvider, "anthropic") + strings.Replace(federated, "  credential:\n", "  credential:\n"+extra, 1)
		if e := resolveErr(t, body, on); e.Code != CodeExclusiveFields || e.Paths[0] != "spec.credential.federation" {
			t.Errorf("federation with %s: %s %v", name, e.Code, e.Paths)
		}
	}
	e := resolveErr(t, head(v1.KindProvider, "anthropic")+federated+"  headers:\n    Authorization: Bearer x\n", on)
	if e.Code != CodeReservedPrefix || strings.Join(e.Paths, ",") != `spec.headers["Authorization"]` {
		t.Errorf("a static Authorization header: %s %v", e.Code, e.Paths)
	}
	r := mustResolve(t, head(v1.KindProvider, "anthropic")+strings.Replace(federated, "  credential:\n", "  credential:\n    header: authorization\n    scheme: bearer\n", 1), on)
	if c := r.Object.(*v1.Provider).Spec.Credential; c.Header != "authorization" || c.Scheme != v1.SchemeBearer || c.Federation == nil {
		t.Errorf("an explicit header and scheme: %+v", c)
	}
}
