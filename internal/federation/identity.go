// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/lux/internal/config"
)

// SourceOf is the identity source the configuration names, and what it
// is in words for a start-up line; nil and no error with none.
func SourceOf(w config.WorkloadIdentity, getenv func(string) string) (IdentitySource, string, error) {
	switch {
	case w.File != "":
		return File{Path: w.File}, "read from " + w.File + " for every exchange", nil
	case w.Kubernetes:
		k, err := NewKubernetes(KubernetesOptions{Audience: w.Audience, Getenv: getenv})
		if err != nil {
			return nil, "", fmt.Errorf("LUX_WORKLOAD_IDENTITY_KUBERNETES: %w", err)
		}
		return k, k.Describe(), nil
	default:
		return nil, "", nil
	}
}

// File reads the identity token from Path for every exchange, so a token
// another process keeps fresh on disk is always the current one
// (LUX_WORKLOAD_IDENTITY_FILE).
type File struct {
	Path string
}

// IdentityToken implements IdentitySource.
func (f File) IdentityToken(context.Context) (string, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s is empty", f.Path)
	}
	return token, nil
}

// ServiceAccountDir is where Kubernetes mounts a pod's service account:
// its token, its namespace and the cluster's CA.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// MintedLifetime is how long a token minted for the pod's service
// account lasts: within the one hour Anthropic's issuers accept by
// default, and long enough that the access token it is exchanged for,
// which lasts at most twice the identity token's remaining life, gets a
// rule's default hour.
const MintedLifetime = 30 * time.Minute

// Kubernetes mints a token for the pod's own service account through the
// TokenRequest API for every exchange (LUX_WORKLOAD_IDENTITY_KUBERNETES),
// so each exchange presents a token with a new jti. The service
// account's name is asked of the API server once, with a
// SelfSubjectReview of the pod's own token, at the first exchange.
type Kubernetes struct {
	audience  string
	host      string
	namespace string
	tokenFile string
	client    *http.Client
	mu        sync.Mutex
	endpoint  string
}

// KubernetesOptions is what NewKubernetes reads. Dir is
// ServiceAccountDir and Host the address KUBERNETES_SERVICE_HOST and
// KUBERNETES_SERVICE_PORT name when empty; a test sets both.
type KubernetesOptions struct {
	Audience string
	Dir      string
	Host     string
	Getenv   func(string) string
}

// NewKubernetes reads the pod's mounted service account: its namespace
// and the CA the API server's certificate is checked against. It asks the
// API server nothing.
func NewKubernetes(o KubernetesOptions) (*Kubernetes, error) {
	if o.Dir == "" {
		o.Dir = ServiceAccountDir
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Host == "" {
		host, port := o.Getenv("KUBERNETES_SERVICE_HOST"), o.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are unset; the gateway is not running in a pod")
		}
		o.Host = "https://" + net.JoinHostPort(host, port)
	}
	namespace, err := os.ReadFile(filepath.Join(o.Dir, "namespace"))
	if err != nil {
		return nil, fmt.Errorf("the pod's namespace: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(o.Dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("the cluster's CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the cluster's CA holds no certificate")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &Kubernetes{
		audience: o.Audience, host: strings.TrimSuffix(o.Host, "/"), namespace: strings.TrimSpace(string(namespace)),
		tokenFile: filepath.Join(o.Dir, "token"),
		client:    &http.Client{Transport: otel.Transport(transport), Timeout: exchangeTimeout},
	}, nil
}

// Describe names where the tokens are minted, for the start-up line.
func (k *Kubernetes) Describe() string {
	return "minted for every exchange for the pod's service account in namespace " + k.namespace + " at " + k.host + ", audience " + k.audience
}

// post sends body to path on the API server with the pod's own token,
// read again each time since the kubelet rotates it, and decodes the
// answer into out when it is one of ok.
func (k *Kubernetes) post(ctx context.Context, path string, body any, out any, ok ...int) error {
	own, err := File{Path: k.tokenFile}.IdentityToken(ctx)
	if err != nil {
		return fmt.Errorf("the pod's service account token: %w", err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.host+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+own)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if !slices.Contains(ok, resp.StatusCode) {
		var status struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(answer, &status)
		return fmt.Errorf("the API server answered %d: %s", resp.StatusCode, status.Message)
	}
	return json.Unmarshal(answer, out)
}

// tokenEndpoint is the TokenRequest route of the pod's own service
// account, its name asked of the API server once.
func (k *Kubernetes) tokenEndpoint(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.endpoint != "" {
		return k.endpoint, nil
	}
	var review struct {
		Status struct {
			UserInfo struct {
				Username string `json:"username"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	body := map[string]string{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}
	if err := k.post(ctx, "/apis/authentication.k8s.io/v1/selfsubjectreviews", body, &review, http.StatusCreated, http.StatusOK); err != nil {
		return "", fmt.Errorf("who the pod's service account is: %w", err)
	}
	user := review.Status.UserInfo.Username
	rest, isAccount := strings.CutPrefix(user, "system:serviceaccount:")
	ns, name, found := strings.Cut(rest, ":")
	if !isAccount || !found || name == "" {
		return "", fmt.Errorf("the pod's token is of %q, not a service account", user)
	}
	if ns != k.namespace {
		return "", fmt.Errorf("the pod's token is of namespace %q, the pod's is %q", ns, k.namespace)
	}
	k.endpoint = "/api/v1/namespaces/" + url.PathEscape(ns) + "/serviceaccounts/" + url.PathEscape(name) + "/token"
	return k.endpoint, nil
}

// IdentityToken implements IdentitySource: a TokenRequest for the pod's
// own service account with the configured audience.
func (k *Kubernetes) IdentityToken(ctx context.Context) (string, error) {
	endpoint, err := k.tokenEndpoint(ctx)
	if err != nil {
		return "", err
	}
	request := map[string]any{
		"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest",
		"spec": map[string]any{"audiences": []string{k.audience}, "expirationSeconds": int64(MintedLifetime / time.Second)},
	}
	var got struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := k.post(ctx, endpoint, request, &got, http.StatusCreated, http.StatusOK); err != nil {
		return "", fmt.Errorf("a token for the pod's service account: %w", err)
	}
	if got.Status.Token == "" {
		return "", errors.New("a token for the pod's service account: the API server answered without a token")
	}
	return got.Status.Token, nil
}
