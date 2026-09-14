// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"flag"
	"net/url"
	"strings"
	"time"
)

// usageOptions are the filters both usage routes take, and the members
// each adds: by and interval for usage, status, error, and limit for
// requests.
type usageOptions struct {
	keys, models, providers, owners, labels, by multi
	since, from, to, interval                   string
	status, errorCode                           string
	limit                                       int
}

func usageFilters(fs *flag.FlagSet) *usageOptions {
	o := &usageOptions{}
	fs.Var(&o.keys, "key", "a Key, by name or id; repeatable")
	fs.Var(&o.models, "model", "a Model, by name or id; repeatable")
	fs.Var(&o.providers, "provider", "a Provider, by name or id; repeatable")
	fs.Var(&o.owners, "owner", "an owner; repeatable")
	fs.Var(&o.labels, "label", "a Key label, k=v; repeatable")
	fs.StringVar(&o.since, "since", "", "a duration back from now, such as 24h; the same as -from now-24h")
	fs.StringVar(&o.from, "from", "", "the start of the range, RFC 3339")
	fs.StringVar(&o.to, "to", "", "the end of the range, RFC 3339")
	return o
}

func usageFlags(_ *app, fs *flag.FlagSet) any {
	o := usageFilters(fs)
	fs.Var(&o.by, "by", "a dimension to group by: key, model, provider, owner, door, status, or label:<k>; repeatable, at most three")
	fs.StringVar(&o.interval, "interval", "", "the bucket: hour, day, month, or total")
	return o
}

func requestsFlags(_ *app, fs *flag.FlagSet) any {
	o := usageFilters(fs)
	fs.StringVar(&o.status, "status", "", "ok, refused, or failed")
	fs.StringVar(&o.errorCode, "error", "", "records refused or failed with this code")
	fs.IntVar(&o.limit, "limit", 0, "stop after this many records; 0 is every record")
	return o
}

// query renders the filters as the routes' parameters.
func (a *app) query(o *usageOptions) (url.Values, error) {
	q := url.Values{}
	for _, l := range o.labels {
		if !strings.Contains(l, "=") {
			return nil, &usageError{msg: "-label takes k=v, not " + quote(l) + "."}
		}
	}
	q["key"], q["model"], q["provider"], q["owner"], q["label"] = o.keys, o.models, o.providers, o.owners, o.labels
	for k, v := range q {
		if len(v) == 0 {
			delete(q, k)
		}
	}
	if o.since != "" && o.from != "" {
		return nil, &usageError{msg: "-since and -from both set the start of the range; give one."}
	}
	if o.since != "" {
		d, err := time.ParseDuration(o.since)
		if err != nil || d <= 0 {
			return nil, &usageError{msg: "-since takes a duration such as 24h, not " + quote(o.since) + "."}
		}
		q.Set("from", a.o.Now().Add(-d).UTC().Format(time.RFC3339))
	}
	if o.from != "" {
		q.Set("from", o.from)
	}
	if o.to != "" {
		q.Set("to", o.to)
	}
	return q, nil
}

func runUsage(a *app, own any, args []string) error {
	o := own.(*usageOptions)
	if err := want("usage", args, 0, "no argument"); err != nil {
		return err
	}
	q, err := a.query(o)
	if err != nil {
		return err
	}
	if len(o.by) > 0 {
		q["by"] = o.by
	}
	if o.interval != "" {
		q.Set("interval", o.interval)
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	return a.answer(c.Usage(a.ctx, q))
}

func runRequests(a *app, own any, args []string) error {
	o := own.(*usageOptions)
	if err := want("requests", args, 0, "no argument"); err != nil {
		return err
	}
	q, err := a.query(o)
	if err != nil {
		return err
	}
	if o.status != "" {
		q.Set("status", o.status)
	}
	if o.errorCode != "" {
		q.Set("error", o.errorCode)
	}
	if o.limit < 0 {
		return &usageError{msg: "-limit is a count of records and cannot be negative."}
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	return a.answer(c.Requests(a.ctx, q, o.limit))
}
