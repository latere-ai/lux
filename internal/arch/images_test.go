// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two markers of spec 017 that fence the runtime stage every image
// shares, and the three files that carry it.
const (
	stageOpen  = "# >>> shared runtime base <<<"
	stageClose = "# <<< shared runtime base >>>"
)

var imageFiles = []string{"Dockerfile", "Dockerfile.release", "Dockerfile.stubs"}

// runtimeStage is the text between the two markers of one Dockerfile,
// and fails when a marker is missing or repeated.
func runtimeStage(t *testing.T, name, text string) string {
	t.Helper()
	if strings.Count(text, stageOpen) != 1 || strings.Count(text, stageClose) != 1 {
		t.Fatalf("%s: the runtime stage markers must appear exactly once each", name)
	}
	_, rest, _ := strings.Cut(text, stageOpen)
	stage, _, _ := strings.Cut(rest, stageClose)
	return stage
}

// distrolessByDigest is the one base the shared stage may name: the
// distroless static image's nonroot variant, pinned by digest.
var distrolessByDigest = regexp.MustCompile(`(?m)^FROM gcr\.io/distroless/static-debian12:nonroot@sha256:[0-9a-f]{64}$`)

// TestRuntimeStagesMatch is spec 017's rule for the images: Dockerfile,
// Dockerfile.release, and Dockerfile.stubs are byte-identical between
// the shared runtime markers, the stage is distroless pinned by digest
// running as nonroot with both ports exposed, and the two release files
// have no build stage, so a released image differs from a developer's in
// where the binary came from and in nothing else.
func TestRuntimeStagesMatch(t *testing.T) {
	dir := root(t)
	stages := map[string]string{}
	for _, name := range imageFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		stage := runtimeStage(t, name, text)
		stages[name] = stage
		if !distrolessByDigest.MatchString(stage) {
			t.Errorf("%s: the shared stage does not start FROM gcr.io/distroless/static-debian12:nonroot pinned by digest:\n%s", name, stage)
		}
		for _, want := range []string{"\nEXPOSE 8080 8081\n", "\nUSER nonroot:nonroot\n"} {
			if !strings.Contains(stage, want) {
				t.Errorf("%s: the shared stage lacks %q", name, strings.TrimSpace(want))
			}
		}
		if strings.Count(text, "\nFROM ") != strings.Count(text, "\nFROM golang")+1 {
			t.Errorf("%s: every FROM must be the build stage or the shared runtime stage", name)
		}
		if name != "Dockerfile" {
			if strings.Contains(text, "\nRUN ") || strings.Contains(text, "FROM golang") {
				t.Errorf("%s: a release image has no build stage; it copies the binary the pipeline built", name)
			}
			binary := strings.TrimPrefix(strings.TrimPrefix(name, "Dockerfile."), "release")
			if binary == "" {
				binary = "luxd"
			} else {
				binary = "lux-" + binary
			}
			if !strings.Contains(text, "\nARG TARGETARCH\nCOPY dist/"+binary+"_linux_${TARGETARCH} /usr/local/bin/"+binary+"\n") {
				t.Errorf("%s: the binary must be COPY dist/%s_linux_${TARGETARCH}, the file buildx substitutes per platform", name, binary)
			}
			if !strings.Contains(text, "\nENTRYPOINT [\"/usr/local/bin/"+binary+"\"]\n") {
				t.Errorf("%s: the entry point must be /usr/local/bin/%s", name, binary)
			}
		}
	}
	for _, name := range imageFiles[1:] {
		if stages[name] != stages["Dockerfile"] {
			t.Errorf("%s: the runtime stage differs from Dockerfile's between the markers:\n--- Dockerfile\n%s\n--- %s\n%s", name, stages["Dockerfile"], name, stages[name])
		}
	}
}
