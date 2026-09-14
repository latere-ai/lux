// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// The two tables of spec 019 the alerts are checked against: the metric
// table, one row per metric with its type, labels, and owner, and the
// alerts table, one row per rule with its expression and duration.

type metricRow struct {
	name, typ string
	labels    []string
	owner     string
	notBuilt  bool
}

type alertRow struct {
	name, expr, wait string
}

// cells splits one Markdown table row into its trimmed cells.
func cells(line string) []string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// tableRows reads the rows of the Markdown table whose header is head,
// stopping at the first line that is not a row.
func tableRows(t *testing.T, doc, head string) [][]string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, head)
	if !ok {
		t.Fatalf("no table headed %q in the spec", head)
	}
	var rows [][]string
	for i, line := range strings.Split(rest, "\n") {
		if i == 0 {
			continue // the rest of the header line
		}
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "|---") {
			continue
		}
		rows = append(rows, cells(line))
	}
	return rows
}

var backticked = regexp.MustCompile("`([^`]+)`")

// metricTable reads spec 019's metric table.
func metricTable(t *testing.T, doc string) []metricRow {
	t.Helper()
	var out []metricRow
	for _, c := range tableRows(t, doc, "| Metric | Type | Labels | Owner |") {
		if len(c) != 4 {
			t.Fatalf("a metric row with %d cells: %v", len(c), c)
		}
		row := metricRow{name: strings.Trim(c[0], "`"), typ: c[1], owner: c[3], notBuilt: strings.Contains(c[3], "not built")}
		if c[2] != "none" {
			for _, m := range backticked.FindAllStringSubmatch(c[2], -1) {
				row.labels = append(row.labels, m[1])
			}
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		t.Fatal("the metric table is empty")
	}
	return out
}

// alertTable reads spec 019's alerts table.
func alertTable(t *testing.T, doc string) []alertRow {
	t.Helper()
	var out []alertRow
	for _, c := range tableRows(t, doc, "| Name | Expression | For | Means |") {
		if len(c) != 4 {
			t.Fatalf("an alert row with %d cells: %v", len(c), c)
		}
		out = append(out, alertRow{name: strings.Trim(c[0], "`"), expr: strings.Trim(c[1], "`"), wait: c[2]})
	}
	if len(out) == 0 {
		t.Fatal("the alerts table is empty")
	}
	return out
}

// rulesFile is the shape of deploy/base/prometheusrule.yaml this test
// reads: the PrometheusRule kind with one list of groups.
type rulesFile struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	} `yaml:"spec"`
}

// metricRef is one metric name in an expression with the labels its
// selector names.
var metricRef = regexp.MustCompile(`\b(lux_[a-z_]+?)(_bucket|_sum|_count)?(\{([^}]*)\})?(?:\[|\)|\s|$)`)

// TestAlertsNameKnownMetrics is spec 019's row for the rules file: the
// file parses as a PrometheusRule, carries exactly the alerts of the
// spec's table with the table's expressions and durations, every metric
// an expression names is in the metric table with the histogram's
// suffixes allowed on a histogram alone, every label a selector names
// is a label of that metric's row, and every rule has a severity and a
// summary.
func TestAlertsNameKnownMetrics(t *testing.T) {
	dir := root(t)
	spec, err := os.ReadFile(filepath.Join(dir, "specs", "019-observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	metrics := map[string]metricRow{}
	for _, m := range metricTable(t, string(spec)) {
		metrics[m.name] = m
	}
	alerts := alertTable(t, string(spec))

	data, err := os.ReadFile(filepath.Join(dir, "deploy", "base", "prometheusrule.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file rulesFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatalf("the rules file does not parse: %v", err)
	}
	if file.APIVersion != "monitoring.coreos.com/v1" || file.Kind != "PrometheusRule" || len(file.Spec.Groups) != 1 {
		t.Fatalf("not one PrometheusRule group: %s %s %d", file.APIVersion, file.Kind, len(file.Spec.Groups))
	}
	rules := file.Spec.Groups[0].Rules
	var names []string
	for _, r := range rules {
		names = append(names, r.Alert)
		if r.Labels["severity"] == "" || r.Annotations["summary"] == "" {
			t.Errorf("%s has no severity or no summary", r.Alert)
		}
		i := slices.IndexFunc(alerts, func(a alertRow) bool { return a.name == r.Alert })
		if i < 0 {
			t.Errorf("%s is not in the spec's alerts table", r.Alert)
			continue
		}
		if r.Expr != alerts[i].expr || r.For != alerts[i].wait {
			t.Errorf("%s:\n file %s for %s\n spec %s for %s", r.Alert, r.Expr, r.For, alerts[i].expr, alerts[i].wait)
		}
		for _, m := range metricRef.FindAllStringSubmatch(r.Expr+" ", -1) {
			name, suffix, selector := m[1], m[2], m[4]
			row, ok := metrics[name]
			if !ok {
				t.Errorf("%s names %s, which is not in the metric table", r.Alert, name)
				continue
			}
			if suffix != "" && row.typ != "histogram" {
				t.Errorf("%s reads %s%s of a %s", r.Alert, name, suffix, row.typ)
			}
			for pair := range strings.SplitSeq(selector, ",") {
				label, _, _ := strings.Cut(strings.TrimSpace(pair), "=")
				label = strings.TrimSuffix(label, "!")
				if label == "" || label == "le" && suffix == "_bucket" {
					continue
				}
				if !slices.Contains(row.labels, label) {
					t.Errorf("%s selects %s by %s, which is not a label of its row %v", r.Alert, name, label, row.labels)
				}
			}
		}
	}
	for _, a := range alerts {
		if !slices.Contains(names, a.name) {
			t.Errorf("the spec's %s is not in the rules file", a.name)
		}
	}
	if !strings.Contains(strings.Join(names, " "), "LuxDown") || !regexp.MustCompile(`up\{job="luxd"\}`).MatchString(rules[0].Expr) {
		t.Errorf("the first rule is not LuxDown on the scrape job: %v", names)
	}
}
