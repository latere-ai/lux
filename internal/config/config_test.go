// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: ":8080", InternalAddr: ":8081", DBMaxConns: 8}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":   "127.0.0.1:9000",
		"LUX_INTERNAL_ADDR": "127.0.0.1:9001",
		"LUX_MANIFEST_DIR":  dir,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: "127.0.0.1:9000", InternalAddr: "127.0.0.1:9001", ManifestDir: dir, DBMaxConns: 8}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
	c, err = Load(env(map[string]string{
		"LUX_DB_URL":       "postgres://lux:secret@db.example.com:5432/lux?sslmode=require",
		"LUX_DB_MAX_CONNS": "20",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want = Config{PublicAddr: ":8080", InternalAddr: ":8081", DBURL: "postgres://lux:secret@db.example.com:5432/lux?sslmode=require", DBMaxConns: 20}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReportsEveryProblemInOneSortedMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":   "nope",
		"LUX_INTERNAL_ADDR": "nope",
		"LUX_DB_URL":        "mysql://db.example.com/lux",
		"LUX_DB_MAX_CONNS":  "0",
	}))
	if err == nil {
		t.Fatal("Load() accepted four bad values")
	}
	got := err.Error()
	for _, want := range []string{
		"configuration: ",
		`LUX_DB_MAX_CONNS is 0, not between 1 and 100`,
		`LUX_DB_URL has scheme "mysql", not postgres`,
		`LUX_INTERNAL_ADDR is "nope", not a host:port address`,
		`LUX_PUBLIC_ADDR is "nope", not a host:port address`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	// An address that does not parse cannot be compared as a socket, so
	// the equality problem is not reported on top of the two syntax ones.
	if strings.Contains(got, "must differ from") {
		t.Errorf("two unparseable addresses were also reported as one socket:\n%s", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Errorf("the message is more than one line:\n%s", got)
	}
	last := -1
	for _, name := range []string{"LUX_DB_MAX_CONNS", "LUX_DB_URL", "LUX_INTERNAL_ADDR", "LUX_PUBLIC_ADDR"} {
		i := strings.Index(got, name)
		if i < last {
			t.Errorf("problems are not sorted by name:\n%s", got)
		}
		last = i
	}
}

func TestLoadTreatsBlankAsUnset(t *testing.T) {
	c, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "  ", "LUX_INTERNAL_ADDR": "", "LUX_MANIFEST_DIR": " ", "LUX_DB_URL": "\t", "LUX_DB_MAX_CONNS": " "}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr || c.ManifestDir != "" || c.DBURL != "" || c.DBMaxConns != DefaultDBMaxConns {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:9000", "LUX_INTERNAL_ADDR": "127.0.0.1:9000"}))
	if err == nil || !strings.Contains(err.Error(), "must differ from LUX_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:0", "LUX_INTERNAL_ADDR": "127.0.0.1:0"})); err != nil {
		t.Fatal(err)
	}
}

// TestFileModeAndDatabaseAreExclusive is spec 010's row: the two answer
// the same question, so both set is a configuration error naming both.
func TestFileModeAndDatabaseAreExclusive(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LUX_MANIFEST_DIR": t.TempDir(),
		"LUX_DB_URL":       "postgres://db.example.com/lux",
	}))
	if err == nil {
		t.Fatal("Load() accepted a directory and a database together")
	}
	got := err.Error()
	if !strings.Contains(got, "LUX_DB_URL and LUX_MANIFEST_DIR are both set") {
		t.Fatalf("message does not name both:\n%s", got)
	}
	if strings.Count(got, "LUX_DB_URL") != 1 {
		t.Fatalf("a valid URL beside a valid directory is one problem, not two:\n%s", got)
	}
}

func TestManifestDirMustBeAReadableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "provider.yaml")
	if err := os.WriteFile(file, []byte("kind: Provider\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	for name, tc := range map[string]struct{ dir, want string }{
		"a file":         {file, `is "` + file + `", not a directory`},
		"a missing path": {missing, `is "` + missing + `", not a readable directory: stat `},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"LUX_MANIFEST_DIR": tc.dir}))
			if err == nil || !strings.Contains(err.Error(), "LUX_MANIFEST_DIR "+tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestDatabaseURLIsCheckedAndNeverEchoed: the scheme is held to postgres
// and no message carries the value, which may hold a password.
func TestDatabaseURLIsCheckedAndNeverEchoed(t *testing.T) {
	const password = "s3cret-password"
	for name, tc := range map[string]struct{ url, want string }{
		"another scheme": {"mysql://lux:" + password + "@db.example.com/lux", `has scheme "mysql", not postgres`},
		"no scheme":      {"//lux:" + password + "@db.example.com:5432/lux", `has scheme "", not postgres`},
		"not a URL":      {"postgres://lux:" + password + "@db.example.com:port/lux", "does not parse as a URL"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"LUX_DB_URL": tc.url}))
			if err == nil || !strings.Contains(err.Error(), "LUX_DB_URL "+tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("the message echoes the password:\n%s", err)
			}
		})
	}
	for _, ok := range []string{"postgres://db.example.com/lux", "postgresql://db.example.com/lux?sslmode=disable", "POSTGRES://db.example.com/lux"} {
		if _, err := Load(env(map[string]string{"LUX_DB_URL": ok})); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestDBMaxConnsIsBoundedAndReadOnlyWithADatabase(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"below":   {"0", "is 0, not between 1 and 100"},
		"above":   {"101", "is 101, not between 1 and 100"},
		"letters": {"many", `is "many", not an integer`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"LUX_DB_URL": "postgres://db.example.com/lux", "LUX_DB_MAX_CONNS": tc.value}))
			if err == nil || !strings.Contains(err.Error(), "LUX_DB_MAX_CONNS "+tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	for _, edge := range []string{"1", "100"} {
		if _, err := Load(env(map[string]string{"LUX_DB_URL": "postgres://db.example.com/lux", "LUX_DB_MAX_CONNS": edge})); err != nil {
			t.Errorf("%s: %v", edge, err)
		}
	}
	c, err := Load(env(map[string]string{"LUX_DB_MAX_CONNS": "many"}))
	if err != nil || c.DBMaxConns != DefaultDBMaxConns {
		t.Fatalf("without LUX_DB_URL the pool size is not read: %+v, %v", c, err)
	}
}
