// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"bytes"
	"flag"
	"strings"
)

// command is one row of the command table: its words, its usage line,
// the paragraph -help prints, the credential it carries, the flags of
// its own, and what it does.
type command struct {
	name    string
	usage   string
	brief   string
	summary string
	plane   plane
	// setup registers the command's own flags and returns what runs it
	// once they are parsed; a command with no flags of its own leaves it
	// nil and run is called directly.
	setup func(a *app, fs *flag.FlagSet) func(args []string) error
	run   func(a *app, args []string) error
}

// root is the command with no words: its help lists the others.
var root = &command{
	name:  "lux",
	usage: "lux <command> [flags]",
	summary: "The lux command speaks the gateway's /v1 API from a shell or from an\n" +
		"agent: apply a manifest of any kind, read one object, list a kind,\n" +
		"delete, rotate a Key, read usage, ask a door which models a Key may\n" +
		"call, and say who the token belongs to. Output is the server's own\n" +
		"JSON unless -o asks for yaml, table, or wide. Exit 0 means done, 1\n" +
		"means the server refused or could not be reached, and 2 means the\n" +
		"command was wrong and nothing was sent.\n" +
		"\n" +
		"Environment:\n" +
		"  " + EnvURL + "         the gateway's URL (-url)\n" +
		"  " + EnvToken + "       a token from your issuer, for the /v1 commands (-token)\n" +
		"  " + EnvTokenFile + "  a file holding the token, read on every request (-token-file)\n" +
		"  " + EnvKey + "         a Key value, for lux models (-key)\n" +
		"\n" +
		EnvToken + " opens /v1 and " + EnvKey + " opens a door; neither works on the other\n" +
		"side. A door command also reads " + EnvSDKURL + " and " + EnvSDKKey + ", an SDK's\n" +
		"own two variables, when the four above are unset.",
	plane: planeNone,
}

// commands is the table in the order -help and docs/cli.md list it. It
// is filled in init because a command's run reaches the table for its
// usage line, which a package-level literal may not do.
var commands []*command

func init() {
	commands = []*command{
		{
			name:  "apply",
			brief: "apply manifests from files",
			usage: "lux apply -f <file>... [flags]",
			summary: "Apply manifests from files. The kind and the name come from each\n" +
				"document, and a file may hold several YAML documents; they apply in\n" +
				"file order and the first refusal stops the run. A create of a Key\n" +
				"prints its value once, in status.value.",
			plane: planeControl, setup: applyFlags,
		},
		{
			name:  "get",
			brief: "read one object",
			usage: "lux get <kind> <name> [flags]",
			summary: "Read one object with its status. <kind> is provider, model, key, or\n" +
				"budget, singular or plural; <name> is a name or an id.",
			plane: planeControl, run: runGet,
		},
		{
			name:  "list",
			brief: "list the objects of a kind",
			usage: "lux list <kind> [flags]",
			summary: "List the objects of a kind, following every page to the end or to\n" +
				"-limit items, and print one list. -source and -provider apply to\n" +
				"models alone.",
			plane: planeControl, setup: listFlags,
		},
		{
			name:    "delete",
			brief:   "delete one object",
			usage:   "lux delete <kind> <name> [flags]",
			summary: "Delete one object. Nothing is printed on success.",
			plane:   planeControl, setup: deleteFlags,
		},
		{
			name:  "keys rotate",
			brief: "give a Key a new value",
			usage: "lux keys rotate <name> [flags]",
			summary: "Give a Key a new value, keeping its name, id, limits, and budget. The\n" +
				"new value is printed once, in status.value.",
			plane: planeControl, run: runRotate,
		},
		{
			name:  "usage",
			brief: "read usage aggregates",
			usage: "lux usage [flags]",
			summary: "Read usage aggregated over a range, grouped by up to three dimensions\n" +
				"with -by and bucketed with -interval. -since 24h is -from now-24h.",
			plane: planeUsage, setup: usageFlags,
		},
		{
			name:  "requests",
			brief: "read request records",
			usage: "lux requests [flags]",
			summary: "Read the request records themselves, newest first, following every\n" +
				"page to the end or to -limit records.",
			plane: planeUsage, setup: requestsFlags,
		},
		{
			name:  "models",
			brief: "list the models a Key may call, through the door",
			usage: "lux models [flags]",
			summary: "Ask the lux door which models the Key in " + EnvKey + " may call right\n" +
				"now. This is the one command that sends a Key rather than a token.",
			plane: planeDoor, run: runModels,
		},
		{
			name:    "whoami",
			brief:   "say who the token belongs to",
			usage:   "lux whoami [flags]",
			summary: "Say who the token belongs to: the subject, the claims, the policy, and\nthe limits this server holds for it.",
			plane:   planeControl, run: runWhoami,
		},
		{
			name:  "keys create",
			brief: "create a Key from flags",
			usage: "lux keys create <name> -models <m>[,<m>...] [flags]",
			summary: "Build a Key from flags and apply it, printing its value once. -dry-run\n" +
				"prints the manifest instead and sends nothing, which is how a first\n" +
				"file gets written.",
			plane: planeControl, setup: keysCreateFlags,
		},
		{
			name:  "providers create",
			brief: "create a Provider from flags",
			usage: "lux providers create <name> -dialect <d> -base-url <u> [flags]",
			summary: "Build a Provider from flags and apply it. The credential comes from\n" +
				"the environment variable -credential-from-env names and is never\n" +
				"written into the manifest -dry-run prints.",
			plane: planeControl, setup: providersCreateFlags,
		},
		{
			name:  "models create",
			brief: "create a Model from flags",
			usage: "lux models create <name> -target <provider>/<model>[@<weight>[:<priority>]]... [flags]",
			summary: "Build a Model from flags and apply it. -target repeats, one per\n" +
				"provider the model reaches.",
			plane: planeControl, setup: modelsCreateFlags,
		},
		{
			name:  "budgets create",
			brief: "create a Budget from flags",
			usage: "lux budgets create <name> -amount <a> [flags]",
			summary: "Build a Budget from flags and apply it. A budget is hard unless -soft\n" +
				"is given.",
			plane: planeControl, setup: budgetsCreateFlags,
		},
	}
}

// lookup picks the command from the positional words: two words first,
// then one, so `models create` is the flag form and `models` alone is
// the door.
func lookup(words []string) (*command, []string, bool) {
	if len(words) >= 2 {
		if c := byName(words[0] + " " + words[1]); c != nil {
			return c, words[2:], true
		}
	}
	if c := byName(words[0]); c != nil {
		return c, words[1:], true
	}
	return nil, nil, false
}

func byName(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

// help renders one command's -help: the usage line, the summary, the
// command list for the root, and the flags as the flag package prints
// them.
func (a *app) help(cmd *command) string {
	var b strings.Builder
	b.WriteString("Usage: " + cmd.usage + "\n\n" + cmd.summary + "\n\n")
	if cmd == root {
		b.WriteString("Commands:\n")
		for _, c := range commands {
			b.WriteString("  " + pad(c.name, 18) + c.brief + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Flags:\n")
	probe := &app{o: a.o, output: "json"}
	fs := probe.flagSet(cmd.name, cmd.plane != planeUsage)
	if cmd == root {
		fs.BoolVar(&probe.showVersion, "version", false, "print the build identity and exit")
	}
	if cmd.setup != nil {
		cmd.setup(probe, fs)
	}
	var flags bytes.Buffer
	fs.SetOutput(&flags)
	fs.PrintDefaults()
	b.Write(flags.Bytes())
	return b.String()
}

// Doc renders docs/cli.md: the help of the root and of every command, in
// the table's order, so the document and the binary cannot disagree.
func Doc() string {
	a := &app{}
	var b strings.Builder
	b.WriteString("# The lux command\n\n")
	b.WriteString("What `lux -help` prints, command by command. A test holds this file\n")
	b.WriteString("equal to the binary's own output, so what is written here is what the\n")
	b.WriteString("command says.\n\n")
	b.WriteString("## lux\n\n```\n" + a.help(root) + "```\n")
	for _, c := range commands {
		b.WriteString("\n## lux " + c.name + "\n\n```\n" + a.help(c) + "```\n")
	}
	return b.String()
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}
