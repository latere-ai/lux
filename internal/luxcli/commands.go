// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"flag"
	"net/url"
	"strconv"
	"strings"

	"latere.ai/x/lux/internal/luxclient"
)

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// kinds maps the words a caller writes to the route's plural.
var kinds = map[string]string{
	"provider": "providers", "providers": "providers",
	"model": "models", "models": "models",
	"key": "keys", "keys": "keys",
	"budget": "budgets", "budgets": "budgets",
}

// kindOf is the plural of a kind word, or a usage error.
func kindOf(word string) (string, error) {
	if p, ok := kinds[word]; ok {
		return p, nil
	}
	return "", &usageError{msg: "The kind is provider, model, key, or budget, not " + quote(word) + "."}
}

// want holds the positional count of a command.
func want(cmd string, args []string, n int, names string) error {
	if len(args) == n {
		return nil
	}
	return &usageError{cmd: byName(cmd), msg: "lux " + cmd + " takes " + names + "."}
}

// answer prints a response in the mode asked and maps the error.
func (a *app) answer(resp *luxclient.Response, err error) error {
	if err != nil {
		return classify(err)
	}
	return a.print(resp.Body)
}

func runGet(a *app, args []string) error {
	if err := want("get", args, 2, "a kind and a name"); err != nil {
		return err
	}
	plural, err := kindOf(args[0])
	if err != nil {
		return err
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	return a.answer(c.Get(a.ctx, plural, args[1]))
}

type listOptions struct {
	labels                  multi
	owner, source, provider string
	limit                   int
}

func listFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &listOptions{}
	fs.Var(&o.labels, "l", "a label selector, k=v; repeatable, every pair must match")
	fs.StringVar(&o.owner, "owner", "", "objects of this owner")
	fs.StringVar(&o.source, "source", "", "models alone: declared or discovered")
	fs.StringVar(&o.provider, "provider", "", "models alone: those with a target on this provider")
	fs.IntVar(&o.limit, "limit", 0, "stop after this many items; 0 is every item")
	return func(args []string) error { return runList(a, o, args) }
}

func runList(a *app, o *listOptions, args []string) error {
	if err := want("list", args, 1, "a kind"); err != nil {
		return err
	}
	plural, err := kindOf(args[0])
	if err != nil {
		return err
	}
	if o.limit < 0 {
		return &usageError{msg: "-limit is a count of items and cannot be negative."}
	}
	q := url.Values{}
	for _, l := range o.labels {
		if !strings.Contains(l, "=") {
			return &usageError{msg: "-l takes k=v, not " + quote(l) + "."}
		}
		q.Add("label", l)
	}
	if o.owner != "" {
		q.Set("owner", o.owner)
	}
	if o.source != "" {
		q.Set("source", o.source)
	}
	if o.provider != "" {
		q.Set("provider", o.provider)
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	return a.answer(c.List(a.ctx, "/v1/"+plural, q, o.limit))
}

type deleteOptions struct{ ifMatch string }

func deleteFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &deleteOptions{}
	fs.StringVar(&o.ifMatch, "if-match", "", "delete only at this version, or * for any")
	return func(args []string) error { return runDelete(a, o, args) }
}

func runDelete(a *app, o *deleteOptions, args []string) error {
	if err := want("delete", args, 2, "a kind and a name"); err != nil {
		return err
	}
	plural, err := kindOf(args[0])
	if err != nil {
		return err
	}
	if err := checkIfMatch(o.ifMatch); err != nil {
		return err
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	_, err = c.Delete(a.ctx, plural, args[1], o.ifMatch)
	return classify(err)
}

// checkIfMatch holds -if-match to * or a version above zero.
func checkIfMatch(v string) error {
	if v == "" || v == "*" {
		return nil
	}
	if n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64); err != nil || n <= 0 {
		return &usageError{msg: "-if-match takes a version above zero or *, not " + quote(v) + "."}
	}
	return nil
}

func runRotate(a *app, args []string) error {
	if err := want("keys rotate", args, 1, "a name"); err != nil {
		return err
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	return a.answer(c.Rotate(a.ctx, args[0]))
}

func runWhoami(a *app, args []string) error {
	if err := want("whoami", args, 0, "no argument"); err != nil {
		return err
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	return a.answer(c.Self(a.ctx))
}

func runModels(a *app, args []string) error {
	if err := want("models", args, 0, "no argument"); err != nil {
		return err
	}
	c, err := a.doorClient()
	if err != nil {
		return err
	}
	return a.answer(c.Models(a.ctx))
}
