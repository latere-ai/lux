// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/lux/internal/config"
)

// The kustomize roots of spec 017: the base and every overlay render,
// and the component composes over an overlay.
var (
	deployRoots  = []string{"deploy/base", "deploy/overlays/kind", "deploy/overlays/generic"}
	hpaComponent = "deploy/components/hpa"
)

// object is one Kubernetes object as YAML decodes it.
type object map[string]any

// kind, name, and dig read the object without a scheme: a deploy
// manifest is checked for the fields the spec names and nothing else.
func (o object) kind() string { return str(o["kind"]) }
func (o object) name() string { return str(dig(o, "metadata", "name")) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

// dig walks nested maps and answers nil at the first missing key.
func dig(v any, path ...string) any {
	for _, key := range path {
		if o, isObject := v.(object); isObject {
			v = map[string]any(o)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v, ok = m[key]
		if !ok {
			return nil
		}
	}
	return v
}

// list is a YAML sequence as a slice, nil for anything else.
func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// decodeDocs splits one YAML stream into its objects, skipping empty
// documents.
func decodeDocs(t *testing.T, name string, data []byte) []object {
	t.Helper()
	var out []object
	for i, doc := range bytes.Split(data, []byte("\n---")) {
		var o object
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatalf("%s: document %d does not parse: %v", name, i+1, err)
		}
		if len(o) > 0 {
			out = append(out, o)
		}
	}
	return out
}

// kustomization is the part of a kustomization.yaml the pure-Go load
// follows: what it includes, and which files its transformers name.
type kustomization struct {
	Resources  []string `yaml:"resources"`
	Components []string `yaml:"components"`
	Patches    []struct {
		Path string `yaml:"path"`
	} `yaml:"patches"`
	ConfigMapGenerator []struct {
		Name string   `yaml:"name"`
		Envs []string `yaml:"envs"`
	} `yaml:"configMapGenerator"`
	Namespace string `yaml:"namespace"`
	Replicas  []struct {
		Name  string `yaml:"name"`
		Count int    `yaml:"count"`
	} `yaml:"replicas"`
	Images []any `yaml:"images"`
}

// loadKustomization is the pure-Go half of a render: it follows every
// resource and component of the kustomization at dir, checks that every
// file a patch or a generator names exists, and returns the objects the
// files carry, unpatched. It proves the tree is whole on a machine with
// no kubectl; the kubectl half proves the transformers.
func loadKustomization(t *testing.T, dir string) (kustomization, []object) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	var k kustomization
	if err := yaml.Unmarshal(data, &k); err != nil {
		t.Fatalf("%s/kustomization.yaml: %v", dir, err)
	}
	var objects []object
	for _, ref := range slices.Concat(k.Resources, k.Components) {
		path := filepath.Join(dir, filepath.FromSlash(ref))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s/kustomization.yaml names %s, which is not in the tree: %v", dir, ref, err)
		}
		if info.IsDir() {
			_, inner := loadKustomization(t, path)
			objects = append(objects, inner...)
			continue
		}
		file, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, decodeDocs(t, path, file)...)
	}
	for _, p := range k.Patches {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p.Path))); err != nil {
			t.Errorf("%s/kustomization.yaml patches %s, which is not in the tree", dir, p.Path)
		}
	}
	for _, g := range k.ConfigMapGenerator {
		for _, env := range g.Envs {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(env))); err != nil {
				t.Errorf("%s/kustomization.yaml generates %s from %s, which is not in the tree", dir, g.Name, env)
			}
		}
	}
	return k, objects
}

// find returns the one object of a kind by name.
func find(t *testing.T, objects []object, kind, name string) object {
	t.Helper()
	for _, o := range objects {
		if o.kind() == kind && o.name() == name {
			return o
		}
	}
	t.Fatalf("no %s named %s among %d objects", kind, name, len(objects))
	return nil
}

// kubectlRender runs kubectl kustomize over dir, or skips the test by
// name when kubectl is not on PATH, as it is not in the hermetic run.
func kubectlRender(t *testing.T, dir string) []object {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not on PATH, so the render is not checked here; the pure-Go load ran")
	}
	cmd := exec.Command("kubectl", "kustomize", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", dir, err, stderr.String())
	}
	return decodeDocs(t, dir, out)
}

// TestOverlaysRender is spec 017's row for the deploy tree: the base
// and every overlay render, and the HPA component composes over an
// overlay. The pure-Go load follows every reference and runs
// everywhere; the kubectl render runs the transformers where kubectl
// is installed and skips by name where it is not.
func TestOverlaysRender(t *testing.T) {
	dir := root(t)
	for _, rel := range deployRoots {
		t.Run(rel, func(t *testing.T) {
			k, objects := loadKustomization(t, filepath.Join(dir, rel))
			find(t, objects, "Deployment", "luxd")
			if strings.HasPrefix(rel, "deploy/overlays/") {
				if k.Namespace == "" {
					t.Error("an overlay names the namespace; the base names none")
				}
				if !slices.Contains(k.Resources, "../../base") {
					t.Errorf("an overlay builds on ../../base, not %v", k.Resources)
				}
				if len(k.ConfigMapGenerator) != 1 || k.ConfigMapGenerator[0].Name != "luxd" {
					t.Error("an overlay generates the ConfigMap luxd from its luxd.env")
				}
			} else if k.Namespace != "" || len(k.Images) != 0 {
				t.Error("the base names no namespace and no image registry; the release pipeline appends the images entry")
			}
			t.Run("kubectl", func(t *testing.T) {
				rendered := kubectlRender(t, filepath.Join(dir, rel))
				d := find(t, rendered, "Deployment", "luxd")
				switch rel {
				case "deploy/overlays/kind":
					if got := dig(d, "spec", "replicas"); fmt.Sprint(got) != "1" {
						t.Errorf("the kind overlay runs one replica, got %v", got)
					}
					s := find(t, rendered, "Service", "luxd")
					if dig(s, "spec", "type") != "NodePort" || fmt.Sprint(dig(list(dig(s, "spec", "ports"))[0], "nodePort")) != "30080" {
						t.Error("the kind overlay serves the public port on NodePort 30080")
					}
				case "deploy/overlays/generic":
					if got := dig(d, "spec", "replicas"); fmt.Sprint(got) != "2" {
						t.Errorf("the generic overlay keeps the base's two replicas, got %v", got)
					}
				}
				if rel != "deploy/base" {
					var generated []string
					for _, o := range rendered {
						if o.kind() == "ConfigMap" && strings.HasPrefix(o.name(), "luxd-") {
							generated = append(generated, o.name())
						}
					}
					var read []string
					for _, e := range list(dig(list(dig(d, "spec", "template", "spec", "containers"))[0], "envFrom")) {
						if n := str(dig(e, "configMapRef", "name")); n != "" {
							read = append(read, n)
						}
					}
					if !slices.Equal(generated, read) {
						t.Errorf("the overlay generates the ConfigMaps %v and the Deployment reads %v; the two must agree", generated, read)
					}
				}
			})
		})
	}
	t.Run(hpaComponent, func(t *testing.T) {
		_, objects := loadKustomization(t, filepath.Join(dir, hpaComponent))
		h := find(t, objects, "HorizontalPodAutoscaler", "luxd")
		if dig(h, "spec", "scaleTargetRef", "name") != "luxd" {
			t.Error("the component scales the Deployment luxd")
		}
		t.Run("kubectl", func(t *testing.T) {
			if _, err := exec.LookPath("kubectl"); err != nil {
				t.Skip("kubectl is not on PATH, so the composition is not rendered here")
			}
			// The temporary directory is resolved through its symlinks,
			// as kustomize resolves every path it is handed, so the
			// relative references below land on the tree.
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			overlay, err := filepath.Rel(tmp, filepath.Join(dir, "deploy", "overlays", "generic"))
			if err != nil {
				t.Fatal(err)
			}
			component, err := filepath.Rel(tmp, filepath.Join(dir, hpaComponent))
			if err != nil {
				t.Fatal(err)
			}
			k := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: lux\nresources: [" + filepath.ToSlash(overlay) + "]\ncomponents: [" + filepath.ToSlash(component) + "]\n"
			if err := os.WriteFile(filepath.Join(tmp, "kustomization.yaml"), []byte(k), 0o644); err != nil {
				t.Fatal(err)
			}
			rendered := kubectlRender(t, tmp)
			find(t, rendered, "HorizontalPodAutoscaler", "luxd")
			find(t, rendered, "Deployment", "luxd")
		})
	})
}

// hardening is spec 016's table over the rendered Deployment, and the
// rest of spec 017's base table: what a Deployment of luxd carries
// wherever it is rendered.
func hardening(t *testing.T, objects []object) {
	t.Helper()
	d := find(t, objects, "Deployment", "luxd")
	pod := dig(d, "spec", "template", "spec")
	if dig(pod, "securityContext", "runAsNonRoot") != true {
		t.Error("the pod runs as non-root")
	}
	if dig(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		t.Error("the pod uses the RuntimeDefault seccomp profile")
	}
	if dig(pod, "automountServiceAccountToken") != false {
		t.Error("the service account token is not mounted")
	}
	if fmt.Sprint(dig(pod, "terminationGracePeriodSeconds")) != "90" {
		t.Errorf("terminationGracePeriodSeconds is %v, want 90 for the drain of spec 002", dig(pod, "terminationGracePeriodSeconds"))
	}
	containers := list(dig(pod, "containers"))
	if len(containers) != 1 {
		t.Fatalf("%d containers, want one", len(containers))
	}
	c := containers[0]
	if dig(c, "securityContext", "allowPrivilegeEscalation") != false {
		t.Error("allowPrivilegeEscalation is not false")
	}
	if dig(c, "securityContext", "readOnlyRootFilesystem") != true {
		t.Error("the root file system is not read-only")
	}
	if dig(c, "securityContext", "runAsNonRoot") != true {
		t.Error("the container does not declare runAsNonRoot")
	}
	if drop := list(dig(c, "securityContext", "capabilities", "drop")); len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("capabilities drop %v, want [ALL]", drop)
	}
	if args := list(dig(c, "args")); len(args) != 1 || args[0] != "serve" {
		t.Errorf("args %v, want [serve]: one role per process", args)
	}
	for _, probe := range []struct{ name, path string }{{"livenessProbe", "/livez"}, {"readinessProbe", "/readyz"}} {
		if dig(c, probe.name, "httpGet", "path") != probe.path || dig(c, probe.name, "httpGet", "port") != "internal" {
			t.Errorf("%s is not GET %s on the internal port", probe.name, probe.path)
		}
	}
	var sources []string
	for _, e := range list(dig(c, "envFrom")) {
		if n := str(dig(e, "configMapRef", "name")); n != "" {
			sources = append(sources, "configmap:"+strings.SplitN(n, "-", 2)[0])
		}
		if n := str(dig(e, "secretRef", "name")); n != "" {
			sources = append(sources, "secret:"+n)
		}
	}
	if !slices.Equal(sources, []string{"configmap:luxd", "secret:luxd-secrets"}) {
		t.Errorf("the configuration comes from the ConfigMap luxd and the Secret luxd-secrets, got %v", sources)
	}
	ports := map[string]bool{}
	for _, p := range list(dig(c, "ports")) {
		ports[str(dig(p, "name"))] = true
	}
	if !ports["public"] || !ports["internal"] {
		t.Errorf("the container names a public and an internal port, got %v", ports)
	}

	sa := find(t, objects, "ServiceAccount", "luxd")
	if sa["automountServiceAccountToken"] != false {
		t.Error("the ServiceAccount mounts its token")
	}
	for _, o := range objects {
		if strings.HasSuffix(o.kind(), "Role") || strings.HasSuffix(o.kind(), "RoleBinding") {
			t.Errorf("%s %s: luxd calls no Kubernetes API and needs no role", o.kind(), o.name())
		}
	}
	np := find(t, objects, "NetworkPolicy", "luxd")
	types := list(dig(np, "spec", "policyTypes"))
	if !slices.Contains(types, any("Ingress")) || !slices.Contains(types, any("Egress")) {
		t.Errorf("the NetworkPolicy holds both directions, got %v", types)
	}
	if len(list(dig(np, "spec", "egress"))) < 3 || len(list(dig(np, "spec", "ingress"))) < 2 {
		t.Error("the NetworkPolicy names DNS, the endpoints, and the other replicas on egress, and the public port and the other replicas on ingress")
	}
	pdb := find(t, objects, "PodDisruptionBudget", "luxd")
	if fmt.Sprint(dig(pdb, "spec", "minAvailable")) != "1" {
		t.Errorf("minAvailable is %v, want 1", dig(pdb, "spec", "minAvailable"))
	}
	find(t, objects, "PrometheusRule", "luxd")
	svc := find(t, objects, "Service", "luxd")
	var names []string
	for _, p := range list(dig(svc, "spec", "ports")) {
		names = append(names, str(dig(p, "name")))
	}
	if !slices.Equal(names, []string{"public", "internal"}) {
		t.Errorf("the Service carries the public and the internal port, got %v", names)
	}
}

// TestBaseIsConfined is spec 016's row for the rendered Deployment and
// spec 017's base table: the hardening fields, the probes on the
// internal port, one role per process, the token not mounted, no Role,
// both directions of the network policy, and the disruption budget.
func TestBaseIsConfined(t *testing.T) {
	dir := root(t)
	_, objects := loadKustomization(t, filepath.Join(dir, "deploy", "base"))
	hardening(t, objects)
	for _, rel := range deployRoots {
		t.Run("kubectl "+rel, func(t *testing.T) {
			hardening(t, kubectlRender(t, filepath.Join(dir, rel)))
		})
	}
}

// TestRolloutDoesNotSurgeThePool is spec 017's connection arithmetic:
// the Deployment rolls with maxSurge 0 and maxUnavailable 1, so a
// rollout never holds more than replicas × LUX_DB_MAX_CONNS connections,
// 16 for the base's two replicas at the default of 8.
func TestRolloutDoesNotSurgeThePool(t *testing.T) {
	dir := root(t)
	_, objects := loadKustomization(t, filepath.Join(dir, "deploy", "base"))
	d := find(t, objects, "Deployment", "luxd")
	replicas := fmt.Sprint(dig(d, "spec", "replicas"))
	if replicas != "2" {
		t.Errorf("the base runs %s replicas, want 2", replicas)
	}
	if dig(d, "spec", "strategy", "type") != "RollingUpdate" {
		t.Fatalf("strategy %v, want RollingUpdate", dig(d, "spec", "strategy", "type"))
	}
	if surge := fmt.Sprint(dig(d, "spec", "strategy", "rollingUpdate", "maxSurge")); surge != "0" {
		t.Errorf("maxSurge is %s: a surge would open a third replica's %d connections against a cluster that caps them in the low tens", surge, config.DefaultDBMaxConns)
	}
	if unavailable := fmt.Sprint(dig(d, "spec", "strategy", "rollingUpdate", "maxUnavailable")); unavailable != "1" {
		t.Errorf("maxUnavailable is %s, want 1, so the roll proceeds with none surging", unavailable)
	}
	t.Logf("a rollout holds at most 2 × %d = %d connections", config.DefaultDBMaxConns, 2*config.DefaultDBMaxConns)
}

// TestDeployNamesNoStub: lux-stubs is a test image and never part of an
// installation, so no file under deploy/ names it; the install job
// applies it as a Pod of its own from the job.
func TestDeployNamesNoStub(t *testing.T) {
	dir := filepath.Join(root(t), "deploy")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, word := range []string{"lux-stubs", "Dockerfile.stubs"} {
			if bytes.Contains(data, []byte(word)) {
				rel, _ := filepath.Rel(dir, path)
				t.Errorf("deploy/%s names %s, which is never part of an installation", rel, word)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
