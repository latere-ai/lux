// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"latere.ai/x/lux/client"
)

// The sentences of the two failures the client raises itself, one code
// each, fixed here as the API's are fixed in its table.
const (
	codeUnreachable = "unreachable"
	codeUnreadable  = "unreadable_response"

	messageUnreachable = "The server could not be reached; check " + EnvURL + " and the network."
	messageUnreadable  = "The server's answer is not one this command can read; check that " + EnvURL + " names a Lux gateway."
	messageRefused     = "The server refused the request."
)

// renderFailure writes one refusal to stderr: the API's sentence
// verbatim, the extra line four codes earn, and under -v the code, the
// paths, the developer detail, and the request id on their own lines.
// No header and no body reach stderr, so no token and no value does.
func (a *app) renderFailure(err error) {
	var (
		refusal     *client.Error
		transport   *client.TransportError
		unreadable  *client.UnreadableError
		code, msg   string
		paths       []string
		detail, rid string
	)
	switch {
	case errors.As(err, &refusal):
		code, msg, paths, detail, rid = refusal.Code, refusal.Message, refusal.Paths, refusal.Detail, refusal.RequestID
		if msg == "" {
			msg = messageRefused
		}
	case errors.As(err, &transport):
		code, msg, detail = codeUnreachable, messageUnreachable, transport.Error()
	case errors.As(err, &unreadable):
		code, msg, detail, rid = codeUnreadable, messageUnreadable, unreadable.Error(), unreadable.RequestID
	default:
		code, msg, detail = codeUnreadable, messageUnreadable, err.Error()
	}
	_, _ = fmt.Fprintln(a.o.Stderr, msg)
	if refusal != nil {
		a.extraLine(refusal)
	}
	if !a.verbose {
		return
	}
	_, _ = fmt.Fprintln(a.o.Stderr, "code: "+code)
	if len(paths) > 0 {
		_, _ = fmt.Fprintln(a.o.Stderr, "paths: "+strings.Join(paths, ", "))
	}
	if detail != "" {
		_, _ = fmt.Fprintln(a.o.Stderr, "detail: "+detail)
	}
	if rid != "" {
		_, _ = fmt.Fprintln(a.o.Stderr, "request: "+rid)
	}
}

// extraLine is the one line more than the sentence that four codes get,
// because their next step is not in the sentence.
func (a *app) extraLine(e *client.Error) {
	switch e.Code {
	case "rate_limited", "spend_exceeded", "budget_exhausted":
		if e.RetryAfter > 0 {
			_, _ = fmt.Fprintln(a.o.Stderr, "retry after "+strconv.Itoa(e.RetryAfter)+" seconds")
		} else {
			_, _ = fmt.Fprintln(a.o.Stderr, "retry later")
		}
	case "conflict":
		_, _ = fmt.Fprintln(a.o.Stderr, "read it again with lux get and re-apply")
	case "unsupported_version":
		if line := a.serverLine(); line != "" {
			_, _ = fmt.Fprintln(a.o.Stderr, line)
		}
	}
}

// serverLine reads /.well-known/lux after an unsupported_version and
// renders the server's version and apiVersion, or nothing when the
// document cannot be read, the refusal having been printed already.
func (a *app) serverLine() string {
	url := first(a.url, a.o.Getenv(EnvURL), a.o.Getenv(EnvSDKURL))
	if url == "" {
		return ""
	}
	resp, err := a.bareClient(url).WellKnown(a.ctx)
	if err != nil {
		return ""
	}
	var doc struct {
		Version    string `json:"version"`
		APIVersion string `json:"apiVersion"`
	}
	if json.Unmarshal(resp.Body, &doc) != nil || doc.APIVersion == "" {
		return ""
	}
	return "server: " + doc.Version + " serves " + doc.APIVersion
}

// tokenFileError classifies a failure of the token source: the file
// named by LUX_TOKEN_FILE could not be read or is empty, which is exit 2
// with the variable named, since no request was made.
func tokenFileError(err error) (error, bool) {
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return &usageError{msg: EnvTokenFile + " names a file that cannot be read."}, true
	}
	if errors.Is(err, client.ErrEmptyToken) {
		return &usageError{msg: EnvTokenFile + " names an empty file."}, true
	}
	return err, false
}

// classify maps a client error to what exit renders: the client's own
// three kinds pass through, and a token source failure is a usage
// error.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var (
		refusal    *client.Error
		transport  *client.TransportError
		unreadable *client.UnreadableError
	)
	if errors.As(err, &refusal) || errors.As(err, &transport) || errors.As(err, &unreadable) {
		return err
	}
	if ue, ok := tokenFileError(err); ok {
		return ue
	}
	return err
}
