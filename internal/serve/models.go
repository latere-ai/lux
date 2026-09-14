// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/lux/gateway"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The models routes of spec 005's dialect table, reached by the discovery
// and health jobs and by nothing else.

// The bounds of one list.
const (
	// maxPages is how many pages a list follows; a longer one is a
	// failed list.
	maxPages = 20
	// maxListBytes bounds one page of a model list read whole.
	maxListBytes = 16 << 20
	// pageSize is the page every paginating dialect is asked for.
	pageSize = "1000"
)

// clientSource is the seam the jobs dial through: *gateway.Clients, and
// spec 004's interface of the same shape.
type clientSource interface {
	Client(ctx context.Context, p *v1.Provider) (*http.Client, error)
}

// candidate is one upstream name a list carried, and whether the upstream
// says it embeds and nothing else.
type candidate struct {
	name      string
	embedding bool
}

// modelsURL is the dialect's models route under the Provider's baseURL,
// with the page token when the dialect paginates and a page is named.
func modelsURL(p *v1.Provider, page string) (string, error) {
	base, err := url.Parse(p.Spec.BaseURL)
	if err != nil {
		return "", fmt.Errorf("baseURL %q: %w", p.Spec.BaseURL, err)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/models"
	q := url.Values{}
	switch p.Spec.Dialect {
	case v1.DialectAnthropic:
		q.Set("limit", pageSize)
		if page != "" {
			q.Set("after_id", page)
		}
	case v1.DialectGemini:
		q.Set("pageSize", pageSize)
		if page != "" {
			q.Set("pageToken", page)
		}
	}
	base.RawQuery = q.Encode()
	return base.String(), nil
}

// upstream is what the jobs need to send one request toward a Provider:
// the client and the credential.
type upstream struct {
	clients     clientSource
	credentials credentialSource
}

// get sends one GET to rawURL through the Provider's client with its
// credential injected, as any request toward it is, and returns the
// value it injected beside the response, so the caller redacts it from
// any excerpt of the upstream's body it keeps: an upstream that echoes
// the header it was sent must not put the credential into a status.
func (u upstream) get(ctx context.Context, p *v1.Provider, rawURL string) (*http.Response, []byte, error) {
	client, err := u.clients.Client(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	value, err := u.credentials.Credential(ctx, p.Status.ID)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	gateway.InjectCredential(req.Header, p, value)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	return resp, value, nil
}

// redact replaces every occurrence of the credential value in an
// upstream body with a marker.
func redact(body, value []byte) []byte {
	if len(value) == 0 {
		return body
	}
	return bytes.ReplaceAll(body, value, []byte("[redacted]"))
}

// listModels reads the Provider's whole model list, page by page, and
// returns every candidate name. An error status, a body that does not
// parse, a list past maxPages, and a transport failure are each a failed
// list.
func (u upstream) listModels(ctx context.Context, p *v1.Provider) ([]candidate, error) {
	var out []candidate
	page := ""
	for n := 1; ; n++ {
		if n > maxPages {
			return nil, fmt.Errorf("the list did not end within %d pages", maxPages)
		}
		rawURL, err := modelsURL(p, page)
		if err != nil {
			return nil, err
		}
		resp, value, err := u.get(ctx, p, rawURL)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", rawURL, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("GET %s: reading the body: %w", rawURL, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: upstream status %d: %s", rawURL, resp.StatusCode, excerpt(redact(body, value)))
		}
		if len(body) > maxListBytes {
			return nil, fmt.Errorf("GET %s: the body is above %d bytes", rawURL, maxListBytes)
		}
		names, next, err := parseList(p.Spec.Dialect, body)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", rawURL, err)
		}
		out = append(out, names...)
		if next == "" {
			return out, nil
		}
		page = next
	}
}

// parseList reads one page in the dialect's shape and returns its
// candidates and the token of the next page, or "" on the last.
func parseList(d v1.Dialect, body []byte) ([]candidate, string, error) {
	switch d {
	case v1.DialectGemini:
		var page struct {
			Models []struct {
				Name    string   `json:"name"`
				Methods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, "", fmt.Errorf("the body is not a models page: %w", err)
		}
		var out []candidate
		for _, m := range page.Models {
			generates, embeds := false, false
			for _, method := range m.Methods {
				switch method {
				case "generateContent":
					generates = true
				case "embedContent":
					embeds = true
				}
			}
			if !generates && !embeds {
				continue
			}
			out = append(out, candidate{name: strings.TrimPrefix(m.Name, "models/"), embedding: embeds && !generates})
		}
		return out, page.NextPageToken, nil
	case v1.DialectAnthropic:
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, "", fmt.Errorf("the body is not a models page: %w", err)
		}
		out := make([]candidate, 0, len(page.Data))
		for _, m := range page.Data {
			out = append(out, candidate{name: m.ID})
		}
		if page.HasMore {
			if page.LastID == "" {
				return nil, "", errors.New("has_more is true and last_id is empty")
			}
			return out, page.LastID, nil
		}
		return out, "", nil
	default:
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, "", fmt.Errorf("the body is not a models list: %w", err)
		}
		out := make([]candidate, 0, len(page.Data))
		for _, m := range page.Data {
			out = append(out, candidate{name: m.ID})
		}
		return out, "", nil
	}
}

// excerpt is the first kilobyte of an upstream body, one line, for the
// developer detail.
func excerpt(body []byte) string {
	const limit = 1024
	if len(body) > limit {
		body = body[:limit]
	}
	return strings.Join(strings.Fields(string(body)), " ")
}

// timeoutOf is the Provider's request deadline: spec.timeout, which
// Resolve fills from the configuration, and the table's default when a
// Provider carries none.
func timeoutOf(p *v1.Provider) time.Duration {
	if d, err := p.Spec.Timeout.Parse(); err == nil && d > 0 {
		return d
	}
	return defaultUpstreamTimeout
}

// defaultUpstreamTimeout is spec 004's default for LUX_UPSTREAM_TIMEOUT,
// for a Provider whose spec.timeout is empty.
const defaultUpstreamTimeout = 10 * time.Minute
