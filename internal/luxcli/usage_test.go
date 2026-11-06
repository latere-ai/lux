// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"flag"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lux/metering"
)

// TestUsageFlagsListTheServersValues holds the values lux usage -help
// offers to the ones GET /v1/usage takes: -interval names every interval
// metering accepts and no other, and -by names every named dimension, the
// label form, and the bound on how many.
func TestUsageFlagsListTheServersValues(t *testing.T) {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	usageFlags(&app{}, fs)

	var intervals []string
	for _, i := range metering.Intervals() {
		intervals = append(intervals, string(i))
	}
	if got := offered(t, fs, "interval", "the bucket: "); !slices.Equal(got, intervals) {
		t.Errorf("-interval offers %q, the server takes %q", got, intervals)
	}
	for _, word := range offered(t, fs, "interval", "the bucket: ") {
		if !metering.Interval(word).Valid() {
			t.Errorf("-interval offers %q, which the server refuses", word)
		}
	}

	var dimensions []string
	for _, d := range metering.Dimensions() {
		dimensions = append(dimensions, string(d))
	}
	dimensions = append(dimensions, metering.LabelPrefix+"<k>")
	if got := offered(t, fs, "by", "a dimension to group by: "); !slices.Equal(got, dimensions) {
		t.Errorf("-by offers %q, the server takes %q", got, dimensions)
	}
	// The help spells the bound out, three, which is the server's MaxBy.
	if by := fs.Lookup("by").Usage; metering.MaxBy != 3 || !strings.HasSuffix(by, "at most three") {
		t.Errorf("-by says %q, and the server takes at most %d", by, metering.MaxBy)
	}
}

// offered is the list a flag's help gives after prefix and before the
// first semicolon, "a, b, or c" read as three values.
func offered(t *testing.T, fs *flag.FlagSet, name, prefix string) []string {
	t.Helper()
	f := fs.Lookup(name)
	if f == nil {
		t.Fatalf("lux usage has no -%s", name)
	}
	list, ok := strings.CutPrefix(f.Usage, prefix)
	if !ok {
		t.Fatalf("-%s says %q, which does not begin %s", name, f.Usage, strconv.Quote(prefix))
	}
	list, _, _ = strings.Cut(list, ";")
	var out []string
	for w := range strings.SplitSeq(list, ", ") {
		out = append(out, strings.TrimPrefix(w, "or "))
	}
	return out
}
