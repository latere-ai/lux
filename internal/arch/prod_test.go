// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// prodKinds is each kind deploy/prod may hold, with the API group and the
// resource the rollout identity's Role grants it under. A kind that is not
// here is cluster-scoped, a Secret, or an RBAC object, which a release must
// not apply: it belongs in deploy/prod-bootstrap.
var prodKinds = map[string][2]string{
	"Deployment":          {"apps", "deployments"},
	"ConfigMap":           {"", "configmaps"},
	"Service":             {"", "services"},
	"ServiceAccount":      {"", "serviceaccounts"},
	"Ingress":             {"networking.k8s.io", "ingresses"},
	"NetworkPolicy":       {"networking.k8s.io", "networkpolicies"},
	"PodDisruptionBudget": {"policy", "poddisruptionbudgets"},
	"CiliumNetworkPolicy": {"cilium.io", "ciliumnetworkpolicies"},
	"PrometheusRule":      {"monitoring.coreos.com", "prometheusrules"},
}

// prodResources is the set of group/resource pairs deploy/prod applies: the
// kind of every object its tree carries, and ConfigMap where it generates one.
func prodResources(t *testing.T) []string {
	t.Helper()
	k, objects := loadKustomization(t, filepath.Join(root(t), "deploy/prod"))
	var out []string
	for _, o := range objects {
		pair, ok := prodKinds[o.kind()]
		if !ok {
			t.Errorf("deploy/prod holds a %s, which the rollout identity does not cover and a release must not apply; keep it in deploy/prod-bootstrap or add it to this test and to rollout-identity.yaml", o.kind())
			continue
		}
		out = append(out, pair[0]+"/"+pair[1])
	}
	if len(k.ConfigMapGenerator) > 0 {
		out = append(out, "/configmaps")
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestRolloutIdentityCoversExactlyWhatProdApplies: the Role allows apply on
// the kinds deploy/prod holds and no others, never touches Secrets or Roles,
// and never deletes; the token Secret is bound to the account it names.
func TestRolloutIdentityCoversExactlyWhatProdApplies(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(root(t), "deploy/prod-bootstrap/rollout-identity.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var granted []string
	for _, o := range decodeDocs(t, "rollout-identity.yaml", data) {
		switch o.kind() {
		case "Role":
			for _, r := range list(o["rules"]) {
				for _, group := range list(dig(r, "apiGroups")) {
					for _, res := range list(dig(r, "resources")) {
						granted = append(granted, str(group)+"/"+str(res))
					}
				}
				for _, verb := range list(dig(r, "verbs")) {
					if !slices.Contains([]string{"get", "list", "create", "update", "patch", "watch"}, str(verb)) {
						t.Errorf("the Role grants the verb %v; a release applies, rolls and reads", verb)
					}
				}
			}
		case "Secret":
			if o.name() != "luxd-rollout-token" || str(o["type"]) != "kubernetes.io/service-account-token" ||
				str(dig(o, "metadata", "annotations", "kubernetes.io/service-account.name")) != "luxd-rollout" {
				t.Errorf("the token Secret %s is not luxd-rollout-token bound to luxd-rollout", o.name())
			}
		}
	}
	slices.Sort(granted)
	granted = slices.Compact(granted)
	if want := prodResources(t); !slices.Equal(granted, want) {
		t.Errorf("the Role grants %v, deploy/prod applies %v", granted, want)
	}
	text := string(data)
	for _, not := range []string{`"secrets"`, `"roles"`, `"rolebindings"`, `"namespaces"`, `"*"`, "ClusterRole"} {
		if strings.Contains(text, not) {
			t.Errorf("the rollout identity names %s; it holds no such grant", not)
		}
	}
	if !strings.Contains(text, `resources: ["deployments"]`+"\n"+`    verbs: ["watch"]`) {
		t.Error("the Role does not let the rollout watch the Deployment")
	}
}

// TestProdTreeKeepsToWhatAReleaseMayApply: what an operator applies is in
// deploy/prod-bootstrap, and no Secret is in either tree.
func TestProdTreeKeepsToWhatAReleaseMayApply(t *testing.T) {
	prodResources(t)
	for _, dir := range []string{"deploy/prod", "deploy/prod-bootstrap"} {
		entries, err := os.ReadDir(filepath.Join(root(t), dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root(t), dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range decodeDocs(t, e.Name(), data) {
				if o.kind() == "Secret" && (dir == "deploy/prod" || o.name() != "luxd-rollout-token") {
					t.Errorf("%s/%s holds the Secret %s; only the rollout identity's token Secret is in a tree, and only in deploy/prod-bootstrap", dir, e.Name(), o.name())
				}
				if dir == "deploy/prod" && (o.kind() == "Role" || o.kind() == "RoleBinding") {
					t.Errorf("deploy/prod holds the %s %s, which the rollout identity may not write", o.kind(), o.name())
				}
			}
		}
	}
	// The pipeline reads deploy/prod alone, and the release archive that
	// readers install from carries neither Latere tree.
	_, text := readWorkflow(t, "release.yml")
	if !strings.Contains(text, "tar --exclude=deploy/prod --exclude=deploy/prod-bootstrap -czf") {
		t.Error("the deploy archive does not leave out deploy/prod and deploy/prod-bootstrap")
	}
}

// TestDeployJobDeploysWhatProdRuns: the deploy job passes the one image
// production runs under the name the overlay gives it, waits for every
// Deployment the overlay renders, and checks the address the smoke script
// takes as its base.
func TestDeployJobDeploysWhatProdRuns(t *testing.T) {
	w, text := readWorkflow(t, "release.yml")
	job := w.Jobs["deploy"]
	if job.If != "${{ vars.LUX_RELEASE_DEPLOY != '' }}" {
		t.Errorf("deploy runs under %q, want the repository variable LUX_RELEASE_DEPLOY alone", job.If)
	}
	if !strings.Contains(text, "DEPLOY_KUBECONFIG: ${{ secrets.LUX_KUBECONFIG }}") {
		t.Error("deploy does not hand the stage the LUX_KUBECONFIG secret")
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Errorf("deploy declares %v, want contents: read alone", job.Permissions)
	}
	needs, _ := job.Needs.([]any)
	for _, n := range []string{"build", "conformance"} {
		if !slices.Contains(needs, any(n)) {
			t.Errorf("deploy does not wait for %s: needs %v", n, needs)
		}
	}
	if got, want := job.With["images"], "luxd=${{ needs.build.outputs.ref_1 }}@${{ needs.build.outputs.digest_1 }}"; got != want {
		t.Errorf("deploy passes the images %q, want %q: the gateway image by digest, named as the overlay names it", got, want)
	}
	if _, ok := job.With["deploy_dir"]; ok {
		t.Error("deploy names a deploy_dir; the stage's default, deploy/prod, is the production tree")
	}
	if got := w.Jobs["publish"].With["evidence"]; got != "${{ needs.deploy.result == 'success' && 'smoke-evidence' || '' }}" {
		t.Errorf("publish names the smoke evidence as %q", got)
	}
	if got := w.Jobs["verify"].If; got != "${{ always() && needs.publish.result == 'success' }}" {
		t.Errorf("verify runs under %q, which a skipped deploy would turn into a skip", got)
	}

	// Every Deployment the overlay renders, one per line.
	rendered := kubectlRender(t, filepath.Join(root(t), "deploy/prod"))
	var deployments []string
	for _, o := range rendered {
		if o.kind() == "Deployment" {
			deployments = append(deployments, o.name())
		}
	}
	var waited []string
	if s, _ := job.With["deployments"].(string); s != "" {
		waited = strings.Fields(s)
	}
	if !slices.Equal(waited, deployments) {
		t.Errorf("deploy waits for %v, deploy/prod renders the Deployments %v", waited, deployments)
	}
	// The address is the public base of the Models API, the origin and the
	// path prefix together, which tools/smoke/release.sh reads as BASE_URL.
	base := str(job.With["url"])
	if !strings.HasSuffix(base, "/v1/models") {
		t.Errorf("deploy checks %q, want the public base of the Models API, ending in /v1/models", base)
	}
	ingress := find(t, rendered, "Ingress", "luxd-core")
	rule := list(dig(ingress, "spec", "rules"))[0]
	path := str(dig(list(dig(rule, "http", "paths"))[0], "path"))
	if !strings.HasSuffix(base, path) || !strings.HasPrefix(base, "https://"+str(dig(rule, "host"))) {
		t.Errorf("deploy checks %q, the Ingress routes %s%s", base, dig(rule, "host"), path)
	}
}

// TestProdTakesTheImageTheStagePins runs the stage's own pin on a copy of
// the tree: after `kustomize edit set image <name>=<ref>@<digest>` in
// deploy/prod, the render of every Deployment runs that exact reference.
// The name is the one the manifests write, so the stage's check that the
// render contains the pin cannot fail on a rename.
func TestProdTakesTheImageTheStagePins(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize is not on PATH, so the stage's pin is not exercised here")
	}
	dir := t.TempDir()
	cp := exec.Command("cp", "-R", filepath.Join(root(t), "deploy"), dir)
	if out, err := cp.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ref := "ghcr.io/example/lux@sha256:" + strings.Repeat("0", 64)
	set := exec.Command("kustomize", "edit", "set", "image", "luxd="+ref)
	set.Dir = filepath.Join(dir, "deploy", "prod")
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("kustomize edit set image: %v\n%s", err, out)
	}
	rendered := kubectlRender(t, set.Dir)
	n := 0
	for _, o := range rendered {
		if o.kind() != "Deployment" {
			continue
		}
		n++
		for _, c := range list(dig(o, "spec", "template", "spec", "containers")) {
			if got := str(dig(c, "image")); got != ref {
				t.Errorf("Deployment %s runs %q after the pin, want %q", o.name(), got, ref)
			}
		}
	}
	if n == 0 {
		t.Error("deploy/prod renders no Deployment")
	}
}
