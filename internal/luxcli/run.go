// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/lux/internal/luxclient"
)

// Options is what Run needs from the process: the arguments after the
// program name, the environment, the two streams, the build identity,
// and, for a test, the clock and the HTTP client.
type Options struct {
	Args   []string
	Getenv func(string) string
	Stdout io.Writer
	Stderr io.Writer
	// Version, Commit, and Date are the build identity -version prints
	// and Version is the User-Agent's.
	Version string
	Commit  string
	Date    string
	// Now is the clock the table renderer and --since read; nil is
	// time.Now.
	Now func() time.Time
	// HTTP is the client every request goes through; nil is
	// luxclient.NewHTTPClient().
	HTTP *http.Client
}

// The three exit codes of spec 014.
const (
	ExitOK      = 0 // the command did what it was asked
	ExitRefused = 1 // the server refused, failed, or could not be reached
	ExitUsage   = 2 // the command was wrong and nothing was sent
)

// The environment variables the command reads, and the two fallbacks a
// door command reads after them, which are latere.ai/x/pkg/luxsdk's
// EnvBaseURL and EnvAPIKey spelled here so the binary does not carry
// that package.
const (
	EnvURL       = "LUX_URL"
	EnvToken     = "LUX_TOKEN"
	EnvTokenFile = "LUX_TOKEN_FILE"
	EnvKey       = "LUX_KEY"
	EnvSDKURL    = "LUX_BASE_URL"
	EnvSDKKey    = "LUX_API_KEY"
)

// app is one invocation: the options and the global flags as parsed.
type app struct {
	o   Options
	ctx context.Context

	url, token, tokenFile, key string
	output                     string
	verbose                    bool
	showVersion                bool
	// set records which flags a FlagSet saw, so a flag whose zero value
	// has a meaning can be told from one left out.
	set map[string]bool
}

// Run parses the arguments, dispatches the command, and returns the exit
// code, writing the answer to stdout and every refusal to stderr.
func Run(ctx context.Context, o Options) int {
	if o.Getenv == nil {
		o.Getenv = func(string) string { return "" }
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = luxclient.NewHTTPClient()
	}
	a := &app{o: o, ctx: ctx, output: "json"}
	return a.exit(a.run())
}

// run is the dispatch: the global flags before the command, the command
// word, then the command's own parse and act.
func (a *app) run() error {
	top := a.flagSet("lux", true)
	top.BoolVar(&a.showVersion, "version", false, "print the build identity and exit")
	if err := top.Parse(a.o.Args); err != nil {
		return a.parseError(root, err)
	}
	rest := top.Args()
	if a.showVersion {
		_, _ = fmt.Fprintf(a.o.Stdout, "lux %s (%s, %s)\n", a.o.Version, a.o.Commit, a.o.Date)
		return nil
	}
	if len(rest) == 0 {
		return &usageError{cmd: root, msg: "A command is required."}
	}
	cmd, args, ok := lookup(rest)
	if !ok {
		return &usageError{cmd: root, msg: "There is no command " + quote(strings.Join(rest[:min(len(rest), 2)], " ")) + "."}
	}
	fs := a.flagSet(cmd.name, cmd.plane != planeUsage)
	runner := func(args []string) error { return cmd.run(a, args) }
	if cmd.setup != nil {
		runner = cmd.setup(a, fs)
	}
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return a.parseError(cmd, err)
	}
	a.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { a.set[f.Name] = true })
	if err := a.checkOutput(); err != nil {
		return err
	}
	return runner(positional)
}

// flagSet is a FlagSet with the global flags. withKey leaves -key out,
// which usage and requests do, since -key there is their own filter.
func (a *app) flagSet(name string, withKey bool) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&a.url, "url", a.url, "the gateway's URL; "+EnvURL+" when unset")
	fs.StringVar(&a.token, "token", a.token, "a token from your issuer, for the /v1 commands; "+EnvToken+" when unset")
	fs.StringVar(&a.tokenFile, "token-file", a.tokenFile, "a file holding the token, read on every request; "+EnvTokenFile+" when unset")
	if withKey {
		fs.StringVar(&a.key, "key", a.key, "a Key value, for lux models; "+EnvKey+" when unset")
	}
	fs.StringVar(&a.output, "o", a.output, "the output: json, yaml, table, or wide")
	fs.BoolVar(&a.verbose, "v", a.verbose, "on a refusal, add the code, the paths, the detail, and the request id")
	return fs
}

// checkOutput holds -o to its four values.
func (a *app) checkOutput() error {
	switch a.output {
	case "json", "yaml", "table", "wide":
		return nil
	}
	return &usageError{msg: "-o takes json, yaml, table, or wide, not " + quote(a.output) + "."}
}

// parseInterleaved parses flags and positional arguments in any order,
// so `lux get key run-42 -o yaml` and `lux get -o yaml key run-42` are
// the same command. -help is the flag package's ErrHelp.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// parseError turns a flag parse failure into help on stdout, exit 0,
// for -help, and a usage error otherwise, carrying the flag package's
// own line, which names the flag.
func (a *app) parseError(cmd *command, err error) error {
	if errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(a.o.Stdout, a.help(cmd))
		return nil
	}
	return &usageError{cmd: cmd, msg: err.Error() + "."}
}

// usageError is exit 2: the sentence, and the command whose usage line
// follows it when there is one.
type usageError struct {
	cmd *command
	msg string
}

func (e *usageError) Error() string { return e.msg }

// exit renders the error to stderr and picks the exit code: nothing for
// nil, 2 for a usage error, 1 for a refusal, a transport failure, or an
// answer the client cannot read.
func (a *app) exit(err error) int {
	if err == nil {
		return ExitOK
	}
	if ue, ok := errors.AsType[*usageError](err); ok {
		_, _ = fmt.Fprintln(a.o.Stderr, ue.msg)
		if ue.cmd != nil {
			_, _ = fmt.Fprintln(a.o.Stderr, "Usage: "+ue.cmd.usage)
		}
		return ExitUsage
	}
	a.renderFailure(err)
	return ExitRefused
}

func quote(s string) string { return `"` + s + `"` }
