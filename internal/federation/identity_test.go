// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/lux/internal/config"
)

// TestFileIdentity is spec 051's fourth criterion, its file half: the
// file is read again for every exchange, and an empty or missing one is
// an error.
func TestFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	f := File{Path: path}
	if _, err := f.IdentityToken(t.Context()); err == nil {
		t.Fatal("a missing file gave a token")
	}
	for _, want := range []string{"jwt-a", "jwt-b"} {
		if err := os.WriteFile(path, []byte(want+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := f.IdentityToken(t.Context()); err != nil || got != want {
			t.Fatalf("%q, %v; want %q", got, err, want)
		}
	}
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.IdentityToken(t.Context()); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("an empty file: %v", err)
	}
}

// TestKubernetesIdentity is spec 051's fourth criterion, its Kubernetes
// half: the service account is asked of the API server once, then each
// token is a TokenRequest for it with the configured audience and a
// 30-minute lifetime, authorized by the pod's mounted token as it is now;
// a token of another namespace, a pod with no API server address, and a
// refusal are errors; SourceOf names the source the configuration does.
func TestKubernetesIdentity(t *testing.T) {
	var mu sync.Mutex
	var reviews, minted []*http.Request
	var bodies []map[string]any
	user, refuse := "system:serviceaccount:latere:luxd-core", false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/apis/authentication.k8s.io/v1/selfsubjectreviews":
			reviews = append(reviews, r.Clone(r.Context()))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"` + user + `"}}}`))
		case "/api/v1/namespaces/latere/serviceaccounts/luxd-core/token":
			if refuse {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"serviceaccounts \"luxd-core\" is forbidden"}`))
				return
			}
			minted, bodies = append(minted, r.Clone(r.Context())), append(bodies, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"token":"minted-` + strconv.Itoa(len(minted)) + `"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("namespace", "latere\n")
	write("ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	write("token", "pod-token-1")

	k, err := NewKubernetes(KubernetesOptions{Audience: "https://api.anthropic.com", Dir: dir, Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.IdentityToken(t.Context()); err != nil || got != "minted-1" {
		t.Fatalf("%q, %v", got, err)
	}
	// The kubelet rotates the mounted token; the next request presents
	// the new one, and the service account is not asked for again.
	write("token", "pod-token-2")
	if got, err := k.IdentityToken(t.Context()); err != nil || got != "minted-2" {
		t.Fatalf("%q, %v", got, err)
	}
	mu.Lock()
	if len(reviews) != 1 || reviews[0].Header.Get("Authorization") != "Bearer pod-token-1" {
		t.Errorf("%d reviews", len(reviews))
	}
	if minted[0].Header.Get("Authorization") != "Bearer pod-token-1" || minted[1].Header.Get("Authorization") != "Bearer pod-token-2" {
		t.Errorf("authorized by %q then %q", minted[0].Header.Get("Authorization"), minted[1].Header.Get("Authorization"))
	}
	spec, _ := bodies[0]["spec"].(map[string]any)
	if bodies[0]["kind"] != "TokenRequest" || spec["expirationSeconds"] != float64(1800) || fmt.Sprint(spec["audiences"]) != "[https://api.anthropic.com]" {
		t.Errorf("the TokenRequest %v", bodies[0])
	}
	refuse = true
	mu.Unlock()
	if _, err := k.IdentityToken(t.Context()); err == nil || !strings.Contains(err.Error(), "answered 403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("a refusal: %v", err)
	}

	mu.Lock()
	user = "system:serviceaccount:other:luxd-core"
	mu.Unlock()
	other, err := NewKubernetes(KubernetesOptions{Dir: dir, Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.IdentityToken(t.Context()); err == nil || !strings.Contains(err.Error(), `namespace "other"`) {
		t.Fatalf("another namespace's token: %v", err)
	}
	if _, err := NewKubernetes(KubernetesOptions{Dir: dir, Getenv: func(string) string { return "" }}); err == nil || !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Fatalf("outside a pod: %v", err)
	}
	if got := k.Describe(); !strings.Contains(got, "namespace latere") || !strings.Contains(got, "audience https://api.anthropic.com") {
		t.Errorf("describe %q", got)
	}
	// The address the pod's environment names, and each file it needs.
	env := func(name string) string {
		return map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1", "KUBERNETES_SERVICE_PORT": "443"}[name]
	}
	if inPod, err := NewKubernetes(KubernetesOptions{Dir: dir, Getenv: env}); err != nil || !strings.Contains(inPod.Describe(), "https://10.0.0.1:443") {
		t.Fatalf("in a pod: %v", err)
	}
	for _, missing := range []string{"namespace", "ca.crt"} {
		bare := t.TempDir()
		for _, f := range []string{"namespace", "ca.crt"} {
			if f != missing {
				b, _ := os.ReadFile(filepath.Join(dir, f))
				_ = os.WriteFile(filepath.Join(bare, f), b, 0o600)
			}
		}
		if _, err := NewKubernetes(KubernetesOptions{Dir: bare, Host: srv.URL}); err == nil {
			t.Errorf("a pod without %s", missing)
		}
	}
	write("ca.crt", "not a certificate")
	if _, err := NewKubernetes(KubernetesOptions{Dir: dir, Host: srv.URL}); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("a CA with no certificate: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	if _, err := k.IdentityToken(t.Context()); err == nil || !strings.Contains(err.Error(), "service account token") {
		t.Errorf("no mounted token: %v", err)
	}
	mu.Lock()
	user = "system:anonymous"
	mu.Unlock()
	write("token", "pod-token-3")
	write("ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	anon, err := NewKubernetes(KubernetesOptions{Dir: dir, Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anon.IdentityToken(t.Context()); err == nil || !strings.Contains(err.Error(), "not a service account") {
		t.Errorf("a token of no service account: %v", err)
	}

	if src, _, err := SourceOf(config.WorkloadIdentity{}, nil); src != nil || err != nil {
		t.Fatalf("no source: %v %v", src, err)
	}
	if src, from, err := SourceOf(config.WorkloadIdentity{File: "/t"}, nil); err != nil || src.(File).Path != "/t" || !strings.Contains(from, "/t") {
		t.Fatalf("a file: %v %q %v", src, from, err)
	}
	if _, _, err := SourceOf(config.WorkloadIdentity{Kubernetes: true}, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "LUX_WORKLOAD_IDENTITY_KUBERNETES") {
		t.Fatalf("Kubernetes outside a pod: %v", err)
	}
}
