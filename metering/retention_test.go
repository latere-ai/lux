// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestParseRetentionRules is LUX_USAGE_RETENTION's grammar: a JSON list
// of rules, the periods a duration or forever with their floors, drop
// among owner, key, and labels once each, no unknown member, and
// nothing set is no rule.
func TestParseRetentionRules(t *testing.T) {
	rules, err := ParseRetentionRules(`[{"match": {"context": "personal"}, "hourly": "2160h", "monthly": "forever", "drop": ["owner", "labels"]}, {"hourly": "forever"}]`)
	if err != nil || len(rules) != 2 {
		t.Fatalf("ParseRetentionRules = %+v, %v", rules, err)
	}
	if rules[0].Hourly != Retention(2160*time.Hour) || rules[0].Monthly != Forever || rules[1].Hourly != Forever || rules[1].Monthly != Forever {
		t.Fatalf("periods = %+v", rules)
	}
	if none, err := ParseRetentionRules("  "); err != nil || none != nil {
		t.Fatalf("an empty value = %v, %v", none, err)
	}
	for raw, want := range map[string]string{
		`{"hourly": "24h"}`:                        "not a JSON list",
		`[{"hourly": "24h", "keep": true}]`:        "unknown field",
		`[{"hourly": "12h"}]`:                      "rule 0: hourly is at least 24h",
		`[{"hourly": "24h"}, {"monthly": "240h"}]`: "rule 1: monthly is at least 720h",
		`[{"drop": ["model"]}]`:                    `drop "model" is not owner, key, or labels`,
		`[{"drop": ["owner", "owner"]}]`:           "drop names a dimension twice",
		`[{"hourly": "soon"}]`:                     "neither a positive duration nor forever",
		`[{"hourly": 24}]`:                         "a retention is a duration",
	} {
		if _, err := ParseRetentionRules(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRetentionRules(%s) = %v, want %q", raw, err, want)
		}
	}
	out, err := json.Marshal(rules[0])
	if err != nil || !strings.Contains(string(out), `"hourly":"2160h0m0s"`) || !strings.Contains(string(out), `"monthly":"forever"`) {
		t.Fatalf("a rule encodes as %s, %v", out, err)
	}
}

// TestRetentionRules: the first rule whose match the labels hold
// applies; the shortest finite periods; when a row is due; and what a
// monthly row keeps under each drop.
func TestRetentionRules(t *testing.T) {
	personal := RetentionRule{Match: map[string]string{"context": "personal"}, Hourly: Retention(90 * 24 * time.Hour), Monthly: Retention(365 * 24 * time.Hour), Drop: []Dimension{DropOwner, DropKey, DropLabels}}
	org := RetentionRule{Match: map[string]string{"context": "org"}, Hourly: Retention(30 * 24 * time.Hour)}
	rules := RetentionRules{personal, org, {}}
	if r := rules.For(map[string]string{"context": "personal", "org": "x"}); r == nil || r.Hourly != personal.Hourly {
		t.Fatalf("For(personal) = %+v", r)
	}
	if r := rules.For(map[string]string{"context": "org"}); r == nil || r.Hourly != org.Hourly {
		t.Fatalf("For(org) = %+v", r)
	}
	if r := rules.For(nil); r == nil || r.Hourly != Forever {
		t.Fatalf("the catch-all = %+v", r)
	}
	if r := (RetentionRules{personal}).For(nil); r != nil {
		t.Fatalf("no match = %+v", r)
	}
	if d, ok := rules.ShortestHourly(); !ok || d != 30*24*time.Hour {
		t.Fatalf("ShortestHourly = %s, %v", d, ok)
	}
	if d, ok := rules.ShortestMonthly(); !ok || d != 365*24*time.Hour {
		t.Fatalf("ShortestMonthly = %s, %v", d, ok)
	}
	if _, ok := (RetentionRules{{}}).ShortestHourly(); ok {
		t.Fatal("a forever rule has a shortest hourly period")
	}
	hour := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	a := Aggregate{Bucket: hour, KeyID: "key_A", Owner: "https://login.example.com|alice", Labels: map[string]string{"context": "personal", "team": "red"}, Requests: 3}
	due := hour.Add(time.Hour).Add(90 * 24 * time.Hour)
	if personal.HourlyDue(a, due.Add(-time.Second)) || !personal.HourlyDue(a, due) {
		t.Fatal("the hourly retention is not measured from the end of the hour")
	}
	var none *RetentionRule
	if none.HourlyDue(a, due.AddDate(10, 0, 0)) || none.MonthlyDue(a, due.AddDate(10, 0, 0)) || (&RetentionRule{}).HourlyDue(a, due.AddDate(10, 0, 0)) {
		t.Fatal("no rule, or a forever rule, made a row due")
	}
	m := personal.MonthlyRow(a)
	if !m.Bucket.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) || m.Owner != "" || m.KeyID != "" || len(m.Labels) != 1 || m.Labels["context"] != "personal" || m.Requests != 3 {
		t.Fatalf("the monthly row = %+v", m)
	}
	if kept := org.MonthlyRow(a); kept.Owner != a.Owner || kept.KeyID != "key_A" || kept.Labels["team"] != "red" {
		t.Fatalf("a rule dropping nothing kept %+v", kept)
	}
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Add(365 * 24 * time.Hour)
	if personal.MonthlyDue(m, end.Add(-time.Second)) || !personal.MonthlyDue(m, end) {
		t.Fatal("the monthly retention is not measured from the end of the month")
	}
	q := Query{From: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)}
	if !q.MonthOverlaps(m) {
		t.Fatal("a range inside the month does not read the month's row")
	}
	q.From, q.To = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	if q.MonthOverlaps(m) {
		t.Fatal("a range after the month read the month's row")
	}
	x, y := AggregateKey{Bucket: 1, KeyID: "a"}, AggregateKey{Bucket: 1, KeyID: "b"}
	if x.Compare(y) != -1 || y.Compare(x) != 1 || x.Compare(x) != 0 || (AggregateKey{Bucket: 2}).Compare(y) != 1 {
		t.Fatal("Compare does not order by bucket then dimensions")
	}
}
