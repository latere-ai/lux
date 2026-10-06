// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// providerReason is the sentence a provider gives for its refusal, the
// part of its answer an operator reads to learn why it refused.
const providerReason = "No endpoints found that support the requested parameters for this model."

// refusalBody is a provider's 400 as an aggregator writes it: a JSON
// error object with the reason first, then metadata that runs the body
// past one KiB.
func refusalBody() string {
	return `{"error":{"message":"` + providerReason + `","code":400,"metadata":{"raw":"` + strings.Repeat("x", 2*maxDetailBytes) + `"}}}`
}

// TestProviderRefusalReasonReachesTheCaller is spec 043 at the door: a
// provider's 400 with a JSON body, on a whole answer and on a stream, is
// upstream_rejected with the provider's status and the first KiB of its
// body in Lux-Error-Detail, cut to the header's bound, and never in the
// caller's body; the request's log line and record carry the upstream
// status.
func TestProviderRefusalReasonReachesTheCaller(t *testing.T) {
	for _, stream := range []bool{false, true} {
		w := newWorld(t)
		body := refusalBody()
		w.openai.respondJSON(http.StatusBadRequest, body)
		rec := w.post("/openai/v1/chat/completions", chatBody("gpt", stream))
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != CodeUpstreamRejected {
			t.Fatalf("stream %v: %d %s", stream, rec.Code, rec.Body.String())
		}
		want := ("upstream status 400: " + body)[:maxDetailBytes]
		if got := rec.Header().Get(HeaderErrorDetail); got != want {
			t.Errorf("stream %v: Lux-Error-Detail\n got %q\nwant %q", stream, got, want)
		}
		if strings.Contains(rec.Body.String(), providerReason) {
			t.Errorf("stream %v: the provider's body reached the caller's: %s", stream, rec.Body.String())
		}
		lines := logLines(t, w.log.String())
		if len(lines) != 1 || lines[0]["upstream_status"] != float64(http.StatusBadRequest) || lines[0]["code"] != string(CodeUpstreamRejected) || lines[0]["status"] != string(StatusFailed) {
			t.Errorf("stream %v: log lines %v", stream, lines)
		}
		if got := w.recorder.last(t).UpstreamStatus; got != http.StatusBadRequest {
			t.Errorf("stream %v: the record's upstream status is %d", stream, got)
		}
	}
}

// TestProviderReasonOnTheLuxDoor: the lux door carries the same detail
// in details.detail, uncut by the header's bound: the status and the
// body's first KiB.
func TestProviderReasonOnTheLuxDoor(t *testing.T) {
	w := newWorld(t)
	body := refusalBody()
	w.openai.respondJSON(http.StatusBadRequest, body)
	rec := w.post("/lux/v1/generate", luxBody("gpt", false))
	if errorCode(t, rec) != CodeUpstreamRejected {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if want := "upstream status 400: " + body[:maxDetailBytes]; env.Error.Details.Detail != want {
		t.Errorf("details.detail\n got %q\nwant %q", env.Error.Details.Detail, want)
	}
}

// TestProviderEchoIsRedacted: an upstream that echoes the credential it
// was sent, in a 400 and in a 429 alike, has it replaced before the
// detail is kept, and a credential-shaped string that is not the
// Provider's is replaced too; the rest of the reason survives.
func TestProviderEchoIsRedacted(t *testing.T) {
	foreign := "sk-" + strings.Repeat("A1b2", 8)
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		w := newWorld(t)
		w.openai.respond(func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(status)
			_, _ = io.WriteString(rw, `{"error":{"message":"`+providerReason+` The request carried `+r.Header.Get("Authorization")+` and `+foreign+`."}}`)
		})
		rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false))
		d := rec.Header().Get(HeaderErrorDetail)
		if strings.Contains(d, openaiCredential) || strings.Contains(d, foreign) {
			t.Errorf("%d: a credential reached the detail: %q", status, d)
		}
		if !strings.Contains(d, "Bearer "+redacted) || !strings.Contains(d, providerReason) {
			t.Errorf("%d: the detail lost the redaction marker or the reason: %q", status, d)
		}
		if strings.Contains(w.log.String(), openaiCredential) {
			t.Errorf("%d: the credential reached the log", status)
		}
	}
}

// TestCredentialAcrossTheCutIsRedacted: a credential that starts inside
// the first KiB and ends past it is read whole and redacted whole, so no
// prefix of it is left at the cut.
func TestCredentialAcrossTheCutIsRedacted(t *testing.T) {
	credential := "sk-provider-" + strings.Repeat("9", 40)
	for _, at := range []int{maxDetailBytes - 10, maxDetailBytes - 1, maxDetailBytes - len(credential)} {
		body := strings.Repeat("a", at) + credential + strings.Repeat("b", 3*maxDetailBytes)
		resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(body))}
		d := upstreamDetail(resp, []byte(credential))
		if strings.Contains(d, credential[:8]) {
			t.Errorf("at %d: a prefix of the credential survived the cut: %q", at, d[len(d)-40:])
		}
		if !strings.HasPrefix(d, "upstream status 400: ") || len(d) != len("upstream status 400: ")+maxDetailBytes {
			t.Errorf("at %d: detail of %d bytes: %q", at, len(d), d[:40])
		}
	}
}

// TestUnreadableBodyIsNamed: a body that fails mid-read keeps what
// arrived and names the read error before it.
func TestUnreadableBodyIsNamed(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(io.MultiReader(strings.NewReader(providerReason), failingReader{}))}
	d := upstreamDetail(resp, nil)
	if !strings.HasPrefix(d, "upstream status 400 (reading the body: ") || !strings.HasSuffix(d, ": "+providerReason) {
		t.Errorf("detail %q", d)
	}
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
