// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// The driver and the migrator, which this package alone imports.
var confined = []string{"github.com/jackc/", "github.com/golang-migrate/"}

// self is the tree the two may be imported under: this package and its
// test helper beneath it.
const self = "latere.ai/x/lux/internal/store/postgres"

// TestDriverIsConfined: no package outside internal/store/postgres,
// its test files and the tier's tagged files included, imports
// github.com/jackc/pgx/v5 or github.com/golang-migrate/migrate/v4
// directly. The build list of the binary reaches them through this
// package alone, which the depcheck gate holds with its rows.
func TestDriverIsConfined(t *testing.T) {
	for _, tags := range []string{"", "postgres", "integration"} {
		args := []string{"list", "-f", `{{.ImportPath}}|{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`, "./..."}
		if tags != "" {
			args = append(args[:1], append([]string{"-tags=" + tags}, args[1:]...)...)
		}
		cmd := exec.Command("go", args...)
		cmd.Dir = "../../.."
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -tags=%q: %v\n%s", tags, err, stderr.String())
		}
		packages := 0
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			pkg, imports, _ := strings.Cut(sc.Text(), "|")
			packages++
			if pkg == self || strings.HasPrefix(pkg, self+"/") {
				continue
			}
			for imp := range strings.FieldsSeq(imports) {
				for _, c := range confined {
					if strings.HasPrefix(imp, c) {
						t.Errorf("-tags=%q: %s imports %s, which only %s may", tags, pkg, imp, self)
					}
				}
			}
		}
		if packages < 10 {
			t.Fatalf("-tags=%q: go list answered %d packages, so nothing was checked", tags, packages)
		}
	}
}
