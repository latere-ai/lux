// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command lux is the agent client of a Lux gateway: it speaks the /v1
// API from a shell or from an agent, applying manifests, reading and
// listing objects, rotating Keys, reading usage, and asking a door which
// models a Key may call. This file is wiring alone: the arguments, the
// environment, the two streams, the signals, and the build identity go
// into internal/luxcli, whose exit code becomes the process's.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"latere.ai/x/lux/internal/luxcli"
	"latere.ai/x/lux/internal/version"
)

// exit is os.Exit, a variable so a test can run main and read the code.
var exit = os.Exit

func main() {
	exit(run(os.Args[1:], os.Getenv))
}

// run wires the process into the command: a context that ends on SIGINT
// or SIGTERM, the streams, and the identity the release pipeline linked
// in, which -version prints as lux <version> (<commit>, <date>).
func run(args []string, getenv func(string) string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return luxcli.Run(ctx, luxcli.Options{
		Args:    args,
		Getenv:  getenv,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version.Version,
		Commit:  version.Commit,
		Date:    version.Date,
	})
}
