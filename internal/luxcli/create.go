// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"encoding/json"
	"errors"
	"flag"
	"strconv"
	"strings"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The four flag forms build a manifest and send it through the same PUT
// as apply, so the server sees one shape. -dry-run prints the manifest
// and sends nothing.

type keysCreateOptions struct {
	models, spend, budget, ttl string
	rpm, tpm                   int
	labels                     multi
	passthrough, dryRun        bool
}

func keysCreateFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &keysCreateOptions{}
	fs.StringVar(&o.models, "models", "", "the model selectors, comma separated; required")
	fs.IntVar(&o.rpm, "rpm", 0, "requests a minute; 0 is no limit")
	fs.IntVar(&o.tpm, "tpm", 0, "tokens a minute; 0 is no limit")
	fs.StringVar(&o.spend, "spend", "", "a spend limit, amount[/window]")
	fs.StringVar(&o.budget, "budget", "", "the Budget the Key draws from")
	fs.StringVar(&o.ttl, "ttl", "", "how long the Key lives, a duration such as 720h")
	fs.Var(&o.labels, "label", "a label, k=v; repeatable")
	fs.BoolVar(&o.passthrough, "passthrough", false, "let the Key call any route the dialect has, translated or not")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the manifest and send nothing")
	return func(args []string) error { return runKeysCreate(a, o, args) }
}

func runKeysCreate(a *app, o *keysCreateOptions, args []string) error {
	if err := want("keys create", args, 1, "a name"); err != nil {
		return err
	}
	if o.models == "" {
		return &usageError{cmd: byName("keys create"), msg: "-models is required."}
	}
	labels, err := pairs(o.labels, "-label")
	if err != nil {
		return err
	}
	k := &v1.Key{Metadata: v1.ObjectMeta{Name: args[0], Labels: labels}}
	for m := range strings.SplitSeq(o.models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			k.Spec.Models = append(k.Spec.Models, m)
		}
	}
	if a.set["rpm"] {
		k.Spec.Limits.RequestsPerMinute = &o.rpm
	}
	if a.set["tpm"] {
		k.Spec.Limits.TokensPerMinute = &o.tpm
	}
	if o.spend != "" {
		amount, window, _ := strings.Cut(o.spend, "/")
		m, err := v1.ParseMoney(amount)
		if err != nil {
			return &usageError{msg: "-spend takes an amount such as 10 or 2.50, then an optional /window, not " + quote(o.spend) + "."}
		}
		k.Spec.Limits.Spend = &v1.Spend{Amount: &m, Window: v1.Window(window)}
	}
	k.Spec.Budget = o.budget
	if o.ttl != "" {
		if _, err := v1.Duration(o.ttl).Parse(); err != nil {
			return &usageError{msg: "-ttl takes a duration such as 720h, not " + quote(o.ttl) + "."}
		}
		k.Spec.TTL = v1.Duration(o.ttl)
	}
	k.Spec.Passthrough = o.passthrough
	return a.create(k, "keys", args[0], o.dryRun, "")
}

type providersCreateOptions struct {
	dialect, baseURL, credential string
	headers                      multi
	dryRun                       bool
}

func providersCreateFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &providersCreateOptions{}
	fs.StringVar(&o.dialect, "dialect", "", "the dialect the provider speaks: openai, anthropic, gemini, or lux; required")
	fs.StringVar(&o.baseURL, "base-url", "", "the provider's base URL; required")
	fs.StringVar(&o.credential, "credential-from-env", "", "NAME: send the variable's value as the credential")
	fs.Var(&o.headers, "header", "a header sent to the provider, k=v; repeatable")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the manifest and send nothing")
	return func(args []string) error { return runProvidersCreate(a, o, args) }
}

func runProvidersCreate(a *app, o *providersCreateOptions, args []string) error {
	if err := want("providers create", args, 1, "a name"); err != nil {
		return err
	}
	if o.dialect == "" || o.baseURL == "" {
		return &usageError{cmd: byName("providers create"), msg: "-dialect and -base-url are required."}
	}
	headers, err := pairs(o.headers, "-header")
	if err != nil {
		return err
	}
	p := &v1.Provider{Metadata: v1.ObjectMeta{Name: args[0]}, Spec: v1.ProviderSpec{Dialect: v1.Dialect(o.dialect), BaseURL: o.baseURL, Headers: headers}}
	value := ""
	if o.credential != "" {
		if value = a.o.Getenv(o.credential); value == "" {
			return &usageError{msg: "The variable " + o.credential + " is unset or empty."}
		}
	}
	return a.create(p, "providers", args[0], o.dryRun, value)
}

type modelsCreateOptions struct {
	targets                          multi
	priceInput, priceOutput, fallbck string
	dryRun                           bool
}

func modelsCreateFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &modelsCreateOptions{}
	fs.Var(&o.targets, "target", "a target, <provider>/<model>[@<weight>[:<priority>]]; repeatable, at least one")
	fs.StringVar(&o.priceInput, "price-input", "", "the price of a million input tokens; with -price-output")
	fs.StringVar(&o.priceOutput, "price-output", "", "the price of a million output tokens; with -price-input")
	fs.StringVar(&o.fallbck, "fallback", "", "onError or never")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the manifest and send nothing")
	return func(args []string) error { return runModelsCreate(a, o, args) }
}

func runModelsCreate(a *app, o *modelsCreateOptions, args []string) error {
	if err := want("models create", args, 1, "a name"); err != nil {
		return err
	}
	if len(o.targets) == 0 {
		return &usageError{cmd: byName("models create"), msg: "-target is required at least once."}
	}
	m := &v1.Model{Metadata: v1.ObjectMeta{Name: args[0]}, Spec: v1.ModelSpec{Fallback: v1.Fallback(o.fallbck)}}
	for _, t := range o.targets {
		target, err := parseTarget(t)
		if err != nil {
			return err
		}
		m.Spec.Targets = append(m.Spec.Targets, target)
	}
	if (o.priceInput == "") != (o.priceOutput == "") {
		return &usageError{msg: "-price-input and -price-output go together."}
	}
	if o.priceInput != "" {
		in, err1 := v1.ParseMoney(o.priceInput)
		out, err2 := v1.ParseMoney(o.priceOutput)
		if err1 != nil || err2 != nil {
			return &usageError{msg: "A price is a decimal amount such as 3 or 0.15."}
		}
		m.Spec.Pricing = &v1.Pricing{Input: &in, Output: &out}
	}
	return a.create(m, "models", args[0], o.dryRun, "")
}

// parseTarget reads <provider>/<model>[@<weight>[:<priority>]].
func parseTarget(s string) (v1.Target, error) {
	bad := &usageError{msg: "-target takes <provider>/<model>[@<weight>[:<priority>]], not " + quote(s) + "."}
	provider, rest, ok := strings.Cut(s, "/")
	if !ok || provider == "" || rest == "" {
		return v1.Target{}, bad
	}
	model, tail, weighted := strings.Cut(rest, "@")
	t := v1.Target{Provider: provider, Model: model}
	if !weighted {
		return t, nil
	}
	weight, priority, prioritized := strings.Cut(tail, ":")
	w, err := strconv.Atoi(weight)
	if err != nil || w < 0 {
		return v1.Target{}, bad
	}
	t.Weight = &w
	if prioritized {
		p, err := strconv.Atoi(priority)
		if err != nil || p < 0 {
			return v1.Target{}, bad
		}
		t.Priority = p
	}
	return t, nil
}

type budgetsCreateOptions struct {
	amount, currency, window string
	soft, dryRun             bool
}

func budgetsCreateFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &budgetsCreateOptions{}
	fs.StringVar(&o.amount, "amount", "", "the amount per window; required")
	fs.StringVar(&o.currency, "currency", "", "the currency; USD when unset")
	fs.StringVar(&o.window, "window", "", "the window: month, none, or a duration; month when unset")
	fs.BoolVar(&o.soft, "soft", false, "warn past the amount instead of refusing")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the manifest and send nothing")
	return func(args []string) error { return runBudgetsCreate(a, o, args) }
}

func runBudgetsCreate(a *app, o *budgetsCreateOptions, args []string) error {
	if err := want("budgets create", args, 1, "a name"); err != nil {
		return err
	}
	if o.amount == "" {
		return &usageError{cmd: byName("budgets create"), msg: "-amount is required."}
	}
	amount, err := v1.ParseMoney(o.amount)
	if err != nil {
		return &usageError{msg: "-amount takes a decimal amount such as 500 or 12.50, not " + quote(o.amount) + "."}
	}
	b := &v1.Budget{Metadata: v1.ObjectMeta{Name: args[0]}, Spec: v1.BudgetSpec{Amount: &amount, Currency: o.currency, Window: v1.Window(o.window)}}
	if o.soft {
		hard := false
		b.Spec.Hard = &hard
	}
	return a.create(b, "budgets", args[0], o.dryRun, "")
}

// create sends a built manifest through the same PUT apply uses, or
// prints it under -dry-run. A credential value goes into the body and
// never into what -dry-run prints.
func (a *app) create(obj v1.Object, plural, name string, dryRun bool, credential string) error {
	o, err := manifestJSON(obj)
	if err != nil {
		return err
	}
	if dryRun {
		return a.printManifest(o)
	}
	body, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if credential != "" {
		p, ok := obj.(*v1.Provider)
		if !ok {
			return errors.New("a credential was given for an object that is not a Provider")
		}
		if body, err = withCredential(p, credential); err != nil {
			return err
		}
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	return a.answer(c.Apply(a.ctx, plural, name, body, manifest.MediaJSON, ""))
}

// pairs reads k=v flags into a map.
func pairs(values multi, flagName string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, v := range values {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, &usageError{msg: flagName + " takes k=v, not " + quote(v) + "."}
		}
		out[k] = val
	}
	return out, nil
}
