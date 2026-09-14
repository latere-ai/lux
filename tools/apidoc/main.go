// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command apidoc writes the OpenAPI document of spec 011 as YAML, the
// file committed at api/openapi.yaml. The document is generated from the
// kinds' Go types, the route table, and the error table of internal/api,
// so the committed file and the served JSON have one source, and
// TestOpenAPIIsCurrent fails the build when the file is stale:
//
//	go run ./tools/apidoc
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"latere.ai/x/lux/internal/api"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run writes the document to the path -o names and returns the exit
// code: 0 when written, 1 when the file could not be written, 2 on a
// usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apidoc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", filepath.Join("api", "openapi.yaml"), "the file to write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	data := api.OpenAPIYAML()
	if err := write(*out, data); err != nil {
		_, _ = fmt.Fprintf(stderr, "apidoc: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "apidoc: wrote %s (%d bytes)\n", *out, len(data))
	return 0
}

// write creates the file's directory and writes the file.
func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
