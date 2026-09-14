// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/store"
)

// TestStoreCannotDecrypt is the half of that criterion the contract
// proves: no package under internal/store reaches the secrets package,
// and Credentials has no method that returns a string or a byte slice,
// so nothing here can hand out a plaintext. The other half, the row
// round trip, is the suite's case of the same name.
func TestStoreCannotDecrypt(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "latere.ai/x/lux/internal/store/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for dep := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(dep, "latere.ai/x/lux/internal/secrets") {
			t.Errorf("internal/store reaches %s", dep)
		}
	}
	creds := reflect.TypeFor[store.Credentials]()
	for m := range creds.Methods() {
		for out := range m.Type.Outs() {
			switch out.Kind() {
			case reflect.String:
				t.Errorf("Credentials.%s returns a string", m.Name)
			case reflect.Slice:
				if out.Elem().Kind() == reflect.Uint8 {
					t.Errorf("Credentials.%s returns bytes", m.Name)
				}
			}
		}
	}
}
