// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"encoding/json"
	"testing"
	"testing/fstest"
	"time"

	"latere.ai/x/lux/metering"
)

// TestFixtureGroupReadsAPreviousRelease drives the fixture group over a
// synthetic release: one resolved manifest per kind, taken from the
// golden corpus, reads back with an equal spec, and a records file
// decodes with every member preserved, while a record whose member the
// current type lacks fails the case, which is the additivity promise the
// group exists to hold. The embedded tree holds no release yet, so the
// group skips there; this is the proof it will bite when one lands.
func TestFixtureGroupReadsAPreviousRelease(t *testing.T) {
	s := startServer(t, serverOptions{})
	c := newClient(t, s.config("alice"))
	t.Cleanup(func() { c.teardown(t) })
	release := fstest.MapFS{
		"testdata/previous/v0.0.1/provider.json": {Data: corpusFile(t, "accepted/provider/anthropic.golden.json")},
		"testdata/previous/v0.0.1/budget.json":   {Data: corpusFile(t, "accepted/budget/team-research.golden.json")},
		"testdata/previous/v0.0.1/model.json":    {Data: corpusFile(t, "accepted/model/claude-sonnet-4.golden.json")},
		"testdata/previous/v0.0.1/key.json":      {Data: corpusFile(t, "accepted/key/run-42.golden.json")},
	}
	rec := metering.Record{ID: "req_01ARZ3NDEKTSV4RRFFQ69G5FAV", At: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC), EndedAt: time.Date(2026, 9, 14, 12, 0, 1, 0, time.UTC),
		Key: metering.KeyRef{ID: "key_01ARZ3NDEKTSV4RRFFQ69G5FAV", Prefix: "lux_abcdefgh"}, Owner: "https://login.example.com|alice",
		Door: "openai", Route: "/openai/v1/chat/completions", Status: metering.StatusOK, Loss: []string{}, Attempts: []metering.Attempt{},
		Tokens: metering.Tokens{Input: 10, Output: 2}, Cost: metering.Charge{Amount: 12, Currency: "USD", Priced: true},
		Labels: map[string]string{}, RequestLabels: map[string]string{}}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	release["testdata/previous/v0.0.1/records.ndjson"] = &fstest.MapFile{Data: append(append([]byte{}, line...), '\n', '\n')}
	c.fixtures = release
	manifests := func(t testing.TB) { case003PreviousReleaseManifests(t, c) }
	records := func(t testing.TB) { case009PreviousReleaseRecords(t, c) }
	if f := drive(t, "fixture/case003PreviousReleaseManifests", manifests); f.Failed() || f.Skipped() {
		t.Errorf("the release's manifests did not read back:\n%s", f.output())
	}
	if f := drive(t, "fixture/case009PreviousReleaseRecords", records); f.Failed() || f.Skipped() {
		t.Errorf("the release's records did not decode whole:\n%s", f.output())
	}

	// A member the current Record lacks is the drift the case exists to
	// catch; a record that does not decode and a file with nothing in it
	// are fixtures that prove nothing; a manifest the server refuses and a
	// file that is missing are named.
	release["testdata/previous/v0.0.1/records.ndjson"] = &fstest.MapFile{Data: []byte(`{"id":"req_01ARZ3NDEKTSV4RRFFQ69G5FAV","vanished":true}` + "\n")}
	if f := drive(t, "fixture/records", records); !f.Failed() {
		t.Error("a record member the current type lacks passed")
	}
	release["testdata/previous/v0.0.1/records.ndjson"] = &fstest.MapFile{Data: []byte("not json\n{\"id\":\"nope\"}\n")}
	if f := drive(t, "fixture/records", records); !f.Failed() {
		t.Error("a record that does not decode passed")
	}
	release["testdata/previous/v0.0.1/records.ndjson"] = &fstest.MapFile{Data: nil}
	if f := drive(t, "fixture/records", records); !f.Failed() {
		t.Error("an empty records file passed")
	}
	delete(release, "testdata/previous/v0.0.1/records.ndjson")
	delete(release, "testdata/previous/v0.0.1/key.json")
	if f := drive(t, "fixture/records", records); !f.Failed() {
		t.Error("a release without records passed")
	}
	if f := drive(t, "fixture/manifests", manifests); !f.Failed() {
		t.Error("a release with a manifest missing passed")
	}
	release["testdata/previous/v0.0.1/key.json"] = &fstest.MapFile{Data: []byte(`{"kind":"Key","metadata":{"name":"k"},"spec":{"models":["*"],"ttl":"1h","expiresAt":"2030-01-01T00:00:00Z"}}`)}
	if f := drive(t, "fixture/manifests", manifests); !f.Failed() {
		t.Error("a release manifest the server refuses passed")
	}
	release["testdata/previous/v0.0.1/key.json"] = &fstest.MapFile{Data: []byte("{")}
	if f := drive(t, "fixture/manifests", manifests); !f.Failed() {
		t.Error("a release manifest that is not JSON passed")
	}
	c.fixtures = fstest.MapFS{"testdata/previous/.gitkeep": {}}
	if f := drive(t, "fixture/manifests", manifests); !f.Skipped() || f.Failed() {
		t.Error("the group did not skip without a release")
	}
	c.fixtures = fstest.MapFS{}
	if f := drive(t, "fixture/manifests", manifests); !f.Failed() {
		t.Error("a tree without the fixtures directory passed")
	}
}
