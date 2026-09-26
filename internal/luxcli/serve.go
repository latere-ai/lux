// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strconv"
	"time"

	"latere.ai/x/lux/client"
	agent "latere.ai/x/lux/client/tunnel"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The reconnect of spec 014: the delay doubles from backoffMin to
// backoffMax and each wait is drawn uniformly below it, which is full
// jitter, so a gateway coming back does not meet every agent at once.
// A session that outlived backoffMax starts the next delay at the
// floor again. The two are variables so a test can drive several
// reconnects in a moment.
var (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
)

// closeMessages is the sentence a person reads per close reason a
// retry cannot fix. The code under -v is spec 013's reason itself, so
// the machine identifier the protocol carries is the one this command
// prints.
var closeMessages = map[string]string{
	agent.ReasonSuperseded:      "Another agent attached this provider, so this session ended.",
	agent.ReasonProviderDeleted: "The provider this session served no longer exists.",
	agent.ReasonTokenExpired:    "The token expired and no fresh one was there to take its place.",
}

// messageClosed is the sentence for a close reason a newer gateway
// sent and this command does not know.
const messageClosed = "The gateway ended this session for a reason this command does not know."

// serveOptions are the flags of spec 014's lux serve row.
type serveOptions struct {
	dialect, upstream, as    string
	include, exclude, labels multi
	carriers                 int
	noApply                  bool
}

func serveFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &serveOptions{}
	fs.StringVar(&o.dialect, "dialect", "", "the dialect the local runtime speaks: openai, anthropic, gemini, or lux; required")
	fs.StringVar(&o.upstream, "upstream", "", "the runtime's base URL on this machine, which stays on this machine; required")
	fs.StringVar(&o.as, "as", "", "the Provider to apply and attach; required")
	fs.Var(&o.include, "include", "a glob of model ids to discover, written into the Provider; repeatable")
	fs.Var(&o.exclude, "exclude", "a glob of model ids to leave out, written into the Provider; repeatable")
	fs.Var(&o.labels, "label", "a label on the Provider, k=v; repeatable")
	fs.IntVar(&o.carriers, "carriers", agent.DefaultCarriers, "streams parked at the gateway")
	fs.BoolVar(&o.noApply, "no-apply", false, "attach to an existing Provider instead of applying one")
	return func(args []string) error { return runServe(a, o, args) }
}

// runServe attaches a local runtime to the gateway: the Provider is
// applied first unless -no-apply, then the session runs until the
// context ends or a close reason a retry cannot fix.
func runServe(a *app, o *serveOptions, args []string) error {
	if err := want("serve", args, 0, "no argument"); err != nil {
		return err
	}
	if o.dialect == "" || o.upstream == "" || o.as == "" {
		return &usageError{cmd: byName("serve"), msg: "lux serve needs -dialect, -upstream, and -as."}
	}
	if !v1.Dialect(o.dialect).Valid() {
		return &usageError{msg: "-dialect takes openai, anthropic, gemini, or lux, not " + quote(o.dialect) + "."}
	}
	if u, err := url.Parse(o.upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &usageError{msg: "-upstream takes the runtime's own URL, such as http://127.0.0.1:11434/v1, not " + quote(o.upstream) + "."}
	}
	if o.carriers < 1 {
		return &usageError{msg: "-carriers is a count of parked streams and is at least 1."}
	}
	labels, err := pairs(o.labels, "-label")
	if err != nil {
		return err
	}
	base := first(a.url, a.o.Getenv(EnvURL))
	if base == "" {
		return &usageError{msg: "Set " + EnvURL + " to the gateway's URL."}
	}
	token, refreshable, err := a.bearerSource()
	if err != nil {
		return err
	}
	c := a.client(base)
	c.Token = token
	// The Provider is applied through the control plane, which may sit in
	// the place of /v1 under LUX_URL's path (spec 040).
	if err := c.Discover(a.ctx); err != nil {
		return err
	}
	if !o.noApply {
		if err := a.applyTunneled(c, o, labels); err != nil {
			return err
		}
	}
	return a.attach(agent.Options{
		Gateway:   c.BaseURL,
		Provider:  o.as,
		Upstream:  o.upstream,
		Token:     token,
		Carriers:  o.carriers,
		UserAgent: "lux/" + a.o.Version,
		Logger:    a.sessionLogger(),
	}, refreshable)
}

// applyTunneled is the PUT that declares the Provider this session
// serves: the dialect, spec.tunnel, the discovery globs, and the
// labels, through the same route lux apply uses. It carries no base
// URL and no credential, because a tunneled runtime is reached over
// the session and holds its own.
func (a *app) applyTunneled(c *client.Client, o *serveOptions, labels map[string]string) error {
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: o.as, Labels: labels},
		Spec: v1.ProviderSpec{
			Dialect:   v1.Dialect(o.dialect),
			Tunnel:    true,
			Discovery: v1.Discovery{Include: o.include, Exclude: o.exclude},
		},
	}
	doc, err := manifestJSON(p)
	if err != nil {
		return err
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return a.answer(c.Apply(a.ctx, "providers", o.as, body, manifest.MediaJSON, ""))
}

// sessionLogger is what the session says while it runs: the developer's
// lines on stderr, at info under -v and at warning otherwise, so a
// reconnect is visible without -v and the frame-by-frame detail is not.
func (a *app) sessionLogger() *slog.Logger {
	level := slog.LevelWarn
	if a.verbose {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(a.o.Stderr, &slog.HandlerOptions{Level: level}))
}

// attach runs one session after another until the context ends, which
// is exit 0, or until a close reason a retry cannot fix, which is exit
// 1 with that reason's sentence. draining reconnects at once and
// token_expired reconnects once with whatever the token file holds
// now; every other end of a session, a broken stream or a gateway that
// cannot be dialed, waits out the backoff and connects again.
func (a *app) attach(opts agent.Options, refreshable bool) error {
	delay, expired := backoffMin, 0
	for {
		started := time.Now()
		err := agent.Run(a.ctx, opts)
		select {
		case <-a.ctx.Done():
			return nil // SIGINT or SIGTERM: the session was closed cleanly
		default:
		}
		if time.Since(started) > backoffMax {
			delay = backoffMin
		}
		var closed *agent.CloseError
		var refused *agent.RefusedError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &refused):
			return refusedFailure(refused)
		case errors.As(err, &closed):
			switch {
			case closed.Reason == agent.ReasonDraining:
				opts.Logger.InfoContext(a.ctx, "lux serve: the gateway is draining; connecting again", "provider", opts.Provider)
				delay, expired = backoffMin, 0
				continue
			case closed.Reason != agent.ReasonTokenExpired:
				return closeFailure(closed.Reason, "the gateway's close frame ended the session and a reconnect would meet the same answer")
			case !refreshable:
				return closeFailure(closed.Reason, "the token came from "+EnvToken+", which a running command cannot refresh; "+EnvTokenFile+" is read again")
			case expired > 0:
				return closeFailure(closed.Reason, "the session was opened again with the token file's current bytes and that token expired too")
			}
			expired++
			opts.Logger.InfoContext(a.ctx, "lux serve: the session's token expired; connecting again with the token file's current bytes", "provider", opts.Provider)
			continue
		}
		if ue, ok := tokenFileError(err); ok {
			return ue
		}
		wait := rand.N(delay)
		opts.Logger.WarnContext(a.ctx, "lux serve: the session ended; connecting again", "provider", opts.Provider, "in", wait, "err", err)
		if !a.pause(wait) {
			return nil
		}
		delay, expired = min(2*delay, backoffMax), 0
	}
}

// pause waits out one backoff, reporting false when the context ended
// first, which is the signal that stops the command.
func (a *app) pause(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// closeFailure renders a close reason as the refusal it is, so the one
// renderer of this package prints it: the reason is the code, its fixed
// sentence is what a person reads, and the detail is for -v.
func closeFailure(reason, detail string) error {
	message, ok := closeMessages[reason]
	if !ok {
		message = messageClosed
	}
	return &client.Error{Code: reason, Message: message, Detail: detail}
}

// refusedFailure renders a connect the gateway refused: the API's own
// code and sentence when the body was the envelope, and this command's
// unreadable_response when it was not.
func refusedFailure(e *agent.RefusedError) error {
	out := &client.Error{Status: e.Status, Code: e.Code, Message: e.Message, Detail: e.Detail}
	if out.Code == "" {
		out.Code, out.Message = codeUnreadable, messageUnreadable
	}
	if out.Message == "" {
		out.Message = messageRefused
	}
	if out.Detail == "" {
		out.Detail = "the gateway answered " + strconv.Itoa(e.Status) + " to the connect"
	}
	return out
}
