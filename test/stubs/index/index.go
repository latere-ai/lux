// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package index is the document a lux-stubs instance serves about
// itself: where every other stub of the run listens, and the credential
// the stub providers check. Each stub has a listener of its own, so a
// caller handed one address cannot find the rest; the index is that one
// address, and a suite driving the stubs reads the document once instead
// of guessing a port.
//
// The document is GET / on the index's own listener, so nothing else
// this tree serves can shadow it, and every other route is a 404 naming
// the one route there is.
package index

import (
	"encoding/json"
	"net/http"
)

// Document names every stub of one lux-stubs run. The members are the
// ones a reader across a process boundary needs: a provider per dialect,
// the three single stubs, and the credential every stub provider checks,
// which a caller writes into the Providers it applies so a request
// forwarded without one is the 401 the stub records.
type Document struct {
	// Providers is one stub provider URL per dialect name.
	Providers map[string]string `json:"providers"`
	// Issuer is the stub issuer's URL.
	Issuer string `json:"issuer"`
	// Authorizer is the stub authorizer's URL.
	Authorizer string `json:"authorizer"`
	// Sink is the stub event sink's URL.
	Sink string `json:"sink"`
	// Credential is the value every stub provider requires in its
	// dialect's credential header.
	Credential string `json:"credential"`
}

// Path is the one route the index serves.
const Path = "/"

// New returns the handler that answers GET / with doc. The body is
// rendered once, so one reader and the next read identical bytes.
func New(doc Document) http.Handler {
	if doc.Providers == nil {
		doc.Providers = map[string]string{}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		// Document holds strings alone, so this cannot happen; a stub
		// that cannot say what it is has nothing to serve.
		panic("index.New: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			http.Error(w, r.URL.Path+" is no route of the stubs index; GET / is the only one", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, r.Method+" "+Path+" is not allowed; the index answers GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	})
}
