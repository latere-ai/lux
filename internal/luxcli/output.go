// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-yaml"
)

// object is a JSON object with its members in the order they arrived, so
// a local rendering keeps the server's field order.
type object struct {
	keys []string
	m    map[string]any
}

func newObject() *object { return &object{m: map[string]any{}} }

func (o *object) set(k string, v any) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// MarshalJSONTo writes the members in order.
func (o *object) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, k := range o.keys {
		if err := enc.WriteToken(jsontext.String(k)); err != nil {
			return err
		}
		if err := json.MarshalEncode(enc, o.m[k]); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// number is a JSON number kept as the digits it arrived with, so an
// integer past 2^53 prints as it was written.
type number string

// MarshalJSONTo writes the digits as a number.
func (n number) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteValue(jsontext.Value(n))
}

// Int64 is the number as an int64, or an error when it is not one.
func (n number) Int64() (int64, error) { return strconv.ParseInt(string(n), 10, 64) }

// Float64 is the number as a float64.
func (n number) Float64() (float64, error) { return strconv.ParseFloat(string(n), 64) }

// responseJSON is how a response body is read: any member order, and
// a repeated name or an invalid byte does not make the body unreadable.
var responseJSON = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// decodeJSON reads one JSON value into *object, []any, string, number,
// bool, and nil.
func decodeJSON(data []byte) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data), responseJSON)
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

func decodeValue(dec *jsontext.Decoder) (any, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case '{':
		o := newObject()
		for dec.PeekKind() != '}' {
			kt, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			k := kt.String()
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			o.set(k, v)
		}
		_, err := dec.ReadToken() // '}'
		return o, err
	case '[':
		out := []any{}
		for dec.PeekKind() != ']' {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		_, err := dec.ReadToken() // ']'
		return out, err
	case '"':
		return tok.String(), nil
	case '0':
		return number(tok.String()), nil
	case 't', 'f':
		return tok.Bool(), nil
	default:
		return nil, nil
	}
}

// toYAML renders a decoded value with the same library the manifest
// package decodes with: members in order, numbers as numbers, and every
// string that would read as another type quoted.
func toYAML(v any) ([]byte, error) {
	return yaml.Marshal(yamlValue(v))
}

func yamlValue(v any) any {
	switch x := v.(type) {
	case *object:
		out := make(yaml.MapSlice, 0, len(x.keys))
		for _, k := range x.keys {
			out = append(out, yaml.MapItem{Key: k, Value: yamlValue(x.m[k])})
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, yamlValue(e))
		}
		return out
	case number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if u, err := strconv.ParseUint(string(x), 10, 64); err == nil {
			return u
		}
		f, _ := x.Float64()
		return f
	default:
		return v
	}
}

// print writes one response body in the mode -o asked: the bytes as
// they arrived for json, and a local rendering otherwise.
func (a *app) print(body []byte) error {
	if a.output == "json" {
		_, err := a.o.Stdout.Write(body)
		return err
	}
	v, err := decodeJSON(body)
	if err != nil {
		return &usageError{msg: messageUnreadable}
	}
	if a.output == "yaml" {
		out, err := toYAML(v)
		if err != nil {
			return err
		}
		_, err = a.o.Stdout.Write(out)
		return err
	}
	_, err = fmt.Fprint(a.o.Stdout, a.table(v, a.output == "wide"))
	return err
}

// printManifest writes a manifest built locally: JSON by default, and
// YAML under -o yaml, table, or wide, since a manifest is a document
// and not a table.
func (a *app) printManifest(v any) error {
	if a.output == "json" {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = a.o.Stdout.Write(append(data, '\n'))
		return err
	}
	out, err := toYAML(v)
	if err != nil {
		return err
	}
	_, err = a.o.Stdout.Write(out)
	return err
}

// column is one table column: its header and the value for one object.
type column struct {
	name string
	get  func(a *app, o *object) string
}

// columns are spec 014's, per kind; wide adds the second slice. No
// column reads a value a manifest is not allowed to carry, so none can
// render one.
var columns = map[string]struct{ base, wide []column }{
	"Provider": {
		base: []column{
			{"NAME", name}, {"DIALECT", str("spec.dialect")}, {"BASEURL", str("spec.baseURL")},
			{"HEALTH", or(str("status.health.state"), "Unknown")}, {"MODELS", or(str("status.discovered.count"), "-")},
			{"AGE", age}, {"OWNER", str("status.owner")},
		},
		wide: []column{{"ID", str("status.id")}, {"CREDENTIAL", credential}, {"TUNNEL", str("status.tunnel.state")}},
	},
	"Model": {
		base: []column{
			{"NAME", name}, {"SOURCE", str("status.source")}, {"TARGETS", targets},
			{"AVAILABLE", or(str("status.available"), "-")}, {"PRICED", priced}, {"AGE", age}, {"OWNER", str("status.owner")},
		},
		wide: []column{{"ID", str("status.id")}, {"CONTEXT", or(str("spec.contextWindow"), "-")}, {"MODALITIES", modalities}},
	},
	"Key": {
		base: []column{
			{"NAME", name}, {"PREFIX", str("status.prefix")}, {"STATE", str("status.state")}, {"MODELS", list("spec.models")},
			{"BUDGET", or(str("status.budget.name"), "-")}, {"EXPIRES", expires}, {"OWNER", str("status.owner")},
		},
		wide: []column{{"ID", str("status.id")}, {"RPM", or(str("spec.limits.requestsPerMinute"), "-")}, {"TPM", or(str("spec.limits.tokensPerMinute"), "-")}, {"SPEND", spend}},
	},
	"Budget": {
		base: []column{
			{"NAME", name}, {"STATE", str("status.state")}, {"AMOUNT", amount}, {"SPENT", or(str("status.spent"), "-")},
			{"REMAINING", or(str("status.remaining"), "-")}, {"RESETS", resets}, {"OWNER", str("status.owner")},
		},
		wide: []column{{"ID", str("status.id")}, {"WINDOW", str("spec.window")}, {"HARD", or(str("spec.hard"), "true")}, {"KEYS", or(str("status.keys"), "-")}},
	},
}

// table renders a value under -o table or wide: the kind's columns for an
// object or a list of one kind, and a generic rendering for anything
// else, a key and a value per line for one object and the first item's
// scalar members as columns for a list.
func (a *app) table(v any, wide bool) string {
	o, ok := v.(*object)
	if !ok {
		return scalar(v) + "\n"
	}
	items, isList := listItems(o)
	if !isList {
		items = []any{o}
	}
	var rows []*object
	for _, it := range items {
		if r, ok := it.(*object); ok {
			rows = append(rows, r)
		}
	}
	kind := ""
	if len(rows) > 0 {
		kind = scalar(rows[0].m["kind"])
	}
	spec, known := columns[kind]
	switch {
	case known:
		cols := spec.base
		if wide {
			cols = append(append([]column{}, spec.base...), spec.wide...)
		}
		out := a.grid(cols, rows)
		if !isList {
			if value := scalar(path(o, "status.value")); value != "" && kind == "Key" {
				out += "key: " + value + "\nThe value is shown once; store it now.\n"
			}
		}
		return out
	case isList:
		if len(rows) == 0 {
			return "\n"
		}
		var cols []column
		for _, k := range rows[0].keys {
			if _, nested := rows[0].m[k].(*object); nested {
				continue
			}
			if _, nested := rows[0].m[k].([]any); nested {
				continue
			}
			cols = append(cols, column{strings.ToUpper(k), str(k)})
		}
		return a.grid(cols, rows)
	default:
		var b strings.Builder
		for _, k := range o.keys {
			b.WriteString(k + ": " + scalar(o.m[k]) + "\n")
		}
		return b.String()
	}
}

// listItems is the items of a list response, or a door's data.
func listItems(o *object) ([]any, bool) {
	for _, k := range []string{"items", "data"} {
		if items, ok := o.m[k].([]any); ok {
			return items, true
		}
	}
	return nil, false
}

func (a *app) grid(cols []column, rows []*object) string {
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	heads := make([]string, len(cols))
	for i, c := range cols {
		heads[i] = c.name
	}
	_, _ = fmt.Fprintln(w, strings.Join(heads, "\t"))
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = c.get(a, r)
		}
		_, _ = fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	_ = w.Flush()
	return b.String()
}

// path reads a dotted member path.
func path(o *object, p string) any {
	var cur any = o
	for seg := range strings.SplitSeq(p, ".") {
		obj, ok := cur.(*object)
		if !ok {
			return nil
		}
		cur = obj.m[seg]
	}
	return cur
}

// scalar renders a value as one cell: a string as it is, a number or a
// bool as its text, nil as empty, and anything nested as compact JSON.
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case number:
		return string(x)
	case bool:
		return strconv.FormatBool(x)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

func str(p string) func(*app, *object) string {
	return func(_ *app, o *object) string { return scalar(path(o, p)) }
}

func or(get func(*app, *object) string, fallback string) func(*app, *object) string {
	return func(a *app, o *object) string {
		if v := get(a, o); v != "" {
			return v
		}
		return fallback
	}
}

func list(p string) func(*app, *object) string {
	return func(_ *app, o *object) string {
		items, _ := path(o, p).([]any)
		parts := make([]string, 0, len(items))
		for _, it := range items {
			parts = append(parts, scalar(it))
		}
		return strings.Join(parts, ",")
	}
}

func name(_ *app, o *object) string { return scalar(path(o, "metadata.name")) }

func age(a *app, o *object) string {
	t, ok := when(path(o, "status.createdAt"))
	if !ok {
		return "-"
	}
	return humanize(a.o.Now().Sub(t))
}

func expires(a *app, o *object) string {
	t, ok := when(path(o, "status.expiresAt"))
	if !ok {
		return "never"
	}
	if d := t.Sub(a.o.Now()); d > 0 {
		return humanize(d)
	}
	return "expired"
}

func resets(a *app, o *object) string {
	t, ok := when(path(o, "status.resetsAt"))
	if !ok {
		return "-"
	}
	if d := t.Sub(a.o.Now()); d > 0 {
		return humanize(d)
	}
	return "now"
}

func credential(_ *app, o *object) string {
	if scalar(path(o, "status.credential.set")) != "true" {
		return "unset"
	}
	return "set v" + scalar(path(o, "status.credential.version"))
}

func targets(_ *app, o *object) string {
	items, _ := path(o, "spec.targets").([]any)
	parts := make([]string, 0, len(items))
	for _, it := range items {
		t, ok := it.(*object)
		if !ok {
			continue
		}
		parts = append(parts, scalar(t.m["provider"])+"/"+scalar(t.m["model"]))
	}
	return strings.Join(parts, ",")
}

func priced(_ *app, o *object) string {
	if _, ok := path(o, "spec.pricing").(*object); ok {
		return "yes"
	}
	return "no"
}

func modalities(a *app, o *object) string {
	return list("spec.modalities.input")(a, o) + "/" + list("spec.modalities.output")(a, o)
}

func spend(_ *app, o *object) string {
	amt := scalar(path(o, "spec.limits.spend.amount"))
	if amt == "" {
		return "-"
	}
	if w := scalar(path(o, "spec.limits.spend.window")); w != "" {
		return amt + "/" + w
	}
	return amt
}

func amount(_ *app, o *object) string {
	return strings.TrimSpace(scalar(path(o, "spec.amount")) + " " + scalar(path(o, "spec.currency")))
}

// when reads an RFC 3339 instant.
func when(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

// humanize renders a duration in the largest whole unit, s, m, h, or d.
func humanize(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}
