// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"testing"
)

func TestProbeBody(t *testing.T) {
	cases := []struct {
		body   string
		want   probe
		wantOK bool
	}{
		{`{"model":"m","stream":true,"max_tokens":10}`, probe{model: "m", hasModel: true, stream: true, maxTokens: 10}, true},
		{`{"model":"m","max_completion_tokens":20}`, probe{model: "m", hasModel: true, maxTokens: 20}, true},
		{`{"model":"m","max_output_tokens":30}`, probe{model: "m", hasModel: true, maxTokens: 30}, true},
		{`{"generationConfig":{"maxOutputTokens":40}}`, probe{maxTokens: 40}, true},
		{`{"model":5,"stream":"yes","max_tokens":"x"}`, probe{}, true},
		{`{"model":""}`, probe{}, true},
		{`{}`, probe{}, true},
		{`[]`, probe{}, false},
		{`"s"`, probe{}, false},
		{`null`, probe{}, false},
		{`{"model":`, probe{}, false},
		{``, probe{}, false},
	}
	for _, c := range cases {
		got, err := probeBody([]byte(c.body))
		if (err == nil) != c.wantOK {
			t.Errorf("%s: err %v, want ok %v", c.body, err, c.wantOK)
			continue
		}
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.body, got, c.want)
		}
	}
}

func TestRewriteModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"a","x":1}`, `{"model":"up","x":1}`},
		{`{ "x" : [1,{"model":"nested"}], "model" : "a" }`, `{ "x" : [1,{"model":"nested"}], "model" : "up" }`},
		{`{"type":"message_start","message":{"id":"i","model":"a"}}`, `{"type":"message_start","message":{"id":"i","model":"up"}}`},
		{`{"x":"a\"model\":\"b\"","model":"a"}`, `{"x":"a\"model\":\"b\"","model":"up"}`},
		{`{"model":1}`, `{"model":1}`},
		{`{"x":1}`, `{"x":1}`},
		{`{"message":"s"}`, `{"message":"s"}`},
		{`[1]`, `[1]`},
		{`{"model":"a"`, `{"model":"up"`}, // the member is found before the missing brace; the probe refused this body already
		{`{"model" "a"}`, `{"model" "a"}`},
		{`{bad:1}`, `{bad:1}`},
		{`{"a":"unterminated}`, `{"a":"unterminated}`},
		{`{"a":[1,2}`, `{"a":[1,2}`},
		{`{"a":}`, `{"a":}`},
		{`{"a":1,}`, `{"a":1,}`},
		{`{"a":1,"b":true,"c":null,"model":"a"}`, `{"a":1,"b":true,"c":null,"model":"up"}`},
	}
	for _, c := range cases {
		if got := string(rewriteModel([]byte(c.in), "up")); got != c.want {
			t.Errorf("rewriteModel(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestSetIncludeUsage(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"m","stream":true}`, `{"stream_options":{"include_usage":true},"model":"m","stream":true}`},
		{`{}`, `{"stream_options":{"include_usage":true}}`},
		{`{ }`, `{"stream_options":{"include_usage":true} }`},
		{`{"stream_options":{}}`, `{"stream_options":{"include_usage":true}}`},
		{`{"stream_options":{"include_usage":false}}`, `{"stream_options":{"include_usage":true}}`},
		{`{"stream_options":{"other":1}}`, `{"stream_options":{"include_usage":true,"other":1}}`},
		{`{"stream_options":{"include_usage":true}}`, `{"stream_options":{"include_usage":true}}`},
		{`{"stream_options":null}`, `{"stream_options":{"include_usage":true}}`},
		{`[1]`, `[1]`},
		{`{"stream_options":{"a":}}`, `{"stream_options":{"a":}}`},
	}
	for _, c := range cases {
		got := string(setIncludeUsage([]byte(c.in)))
		if got != c.want {
			t.Errorf("setIncludeUsage(%s) = %s, want %s", c.in, got, c.want)
		}
		if json.Valid([]byte(c.in)) && !json.Valid([]byte(got)) {
			t.Errorf("setIncludeUsage(%s) = %s is not valid JSON", c.in, got)
		}
	}
}
