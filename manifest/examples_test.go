// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"os"
	"path/filepath"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestExampleModelsCarryTheirFigures: every Model under deploy/examples,
// the ones make run applies, holds an input window and an output limit,
// so each door's model list carries them for a stub as it does for a
// real model, and a client that reads a model's figures from that list
// can use the stub without naming them itself.
func TestExampleModelsCarryTheirFigures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "deploy", "examples", "model-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no example Model under deploy/examples")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := Decode(data, MediaYAML, Hint{})
		if err != nil {
			t.Errorf("%s: Decode: %v", path, err)
			continue
		}
		m, ok := obj.(*v1.Model)
		if !ok {
			t.Errorf("%s is a %T, not a Model", path, obj)
			continue
		}
		if m.Spec.ContextWindow <= 0 || m.Spec.MaxOutputTokens <= 0 {
			t.Errorf("%s: contextWindow %d and maxOutputTokens %d, want both above zero", path, m.Spec.ContextWindow, m.Spec.MaxOutputTokens)
		}
	}
}
