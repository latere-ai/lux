// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/secrets"
)

// env reads m, and answers publicURL for LUX_PUBLIC_URL when m does not
// mention it, since every mode requires the variable and few tests are
// about it; a test about it sets the entry, blank for unset.
func env(m map[string]string) Getenv {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		if k == "LUX_PUBLIC_URL" {
			return publicURLValue
		}
		return ""
	}
}

// issuer and kek are the two variables a server-mode configuration
// cannot do without, and publicURLValue the one every mode needs, so
// every test that is not about them sets them.
const (
	issuer         = "https://login.example.com"
	kek            = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	publicURLValue = "https://lux.example.com"
)

// wantPublicURL is publicURLValue parsed, for the expected configurations.
func wantPublicURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(publicURLValue)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// withKEK adds the key a server-mode configuration needs to m.
func withKEK(m map[string]string) map[string]string {
	out := map[string]string{"LUX_SECRETS_KEK": kek}
	maps.Copy(out, m)
	return out
}

// keyring is kek parsed, for the expected configurations.
func keyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	k, err := secrets.Parse(kek)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PublicAddr: ":8080", InternalAddr: ":8081", DBMaxConns: 8,
		OIDCIssuers: []string{issuer}, OIDCAudiences: []string{"lux"}, AuthorizerTimeout: 5 * time.Second,
		SecretsKEK: keyring(t), DiscoveryInterval: time.Hour, HealthInterval: 30 * time.Second,
		KeyCache: 10 * time.Second, MeteringFlush: time.Second,
		PublicURL: wantPublicURL(t), RequestsPerMinute: 600, UnauthenticatedRequestsPerMinute: 60,
		MaxManifestBytes: 65536, MaxBodyBytes: 64 << 20, UpstreamTimeout: 10 * time.Minute,
		RequestLogExporter: ExporterNone, S3Region: DefaultS3Region, S3Prefix: DefaultS3Prefix,
		TunnelRegistryTTL: 30 * time.Second,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	dir := t.TempDir()
	two, err := secrets.Parse(kek + "," + strings.ReplaceAll(kek, "AQ", "Ag"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":                         "127.0.0.1:9000",
		"LUX_INTERNAL_ADDR":                       "127.0.0.1:9001",
		"LUX_MANIFEST_DIR":                        dir,
		"LUX_OIDC_ISSUERS":                        issuer + "/, http://issuer.internal.example",
		"LUX_OIDC_AUDIENCE":                       "gateway",
		"LUX_OIDC_INSECURE_ISSUERS":               "http://issuer.internal.example/",
		"LUX_AUTHORIZER_URL":                      "https://authz.example.com/decide",
		"LUX_AUTHORIZER_TOKEN":                    " s3cret\n",
		"LUX_AUTHORIZER_TIMEOUT":                  "2s",
		"LUX_ADMIN_SUBJECTS":                      issuer + "|alice, " + issuer + "|ops",
		"LUX_SECRETS_KEK":                         kek + ", " + strings.ReplaceAll(kek, "AQ", "Ag"),
		"LUX_UPSTREAM_ALLOW_PRIVATE":              "1",
		"LUX_DISCOVERY_INTERVAL":                  "15m",
		"LUX_HEALTH_INTERVAL":                     "1m",
		"LUX_KEY_CACHE":                           "30s",
		"LUX_DEFAULT_REQUESTS_PER_MINUTE":         "600",
		"LUX_DEFAULT_TOKENS_PER_MINUTE":           "200000",
		"LUX_PUBLIC_URL":                          "https://gateway.example.com/lux/",
		"LUX_REQUESTS_PER_MINUTE":                 "1200",
		"LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "0",
		"LUX_TRUSTED_PROXIES":                     "10.0.0.0/8, 2001:db8::/32",
		"LUX_MAX_MANIFEST_BYTES":                  "1Mi",
		"LUX_MAX_BODY_BYTES":                      "8Mi",
		"LUX_UPSTREAM_TIMEOUT":                    "2m",
		"LUX_METERING_FLUSH":                      "500ms",
		"LUX_EVENTS_URL":                          "https://sink.example.com/events",
		"LUX_EVENTS_SECRET":                       " whsec-9f8e7d ",
		"LUX_REQUESTLOG_EXPORTER":                 "s3",
		"LUX_S3_ENDPOINT":                         "https://s3.eu-central-1.amazonaws.com",
		"LUX_S3_REGION":                           "eu-central-1",
		"LUX_S3_BUCKET":                           "lux-archive",
		"LUX_S3_ACCESS_KEY":                       "AKIDEXAMPLE",
		"LUX_S3_SECRET_KEY":                       "wJalrXUtnFEMI",
		"LUX_S3_PREFIX":                           "gateways/a/",
		"LUX_TUNNEL_ENABLED":                      "1",
		"LUX_TUNNEL_REGISTRY_TTL":                 "45s",
		"LUX_TUNNEL_FORWARD_ADDR":                 "10.0.0.7:9001",
		"LUX_TUNNEL_FORWARD_SECRET":               secretNew + "," + secretOld,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PublicAddr: "127.0.0.1:9000", InternalAddr: "127.0.0.1:9001", ManifestDir: dir, DBMaxConns: 8,
		OIDCIssuers:                      []string{issuer, "http://issuer.internal.example"},
		OIDCAudiences:                    []string{"gateway"},
		OIDCInsecureIssuers:              []string{"http://issuer.internal.example"},
		AuthorizerURL:                    "https://authz.example.com/decide",
		AuthorizerToken:                  "s3cret",
		AuthorizerTimeout:                2 * time.Second,
		AdminSubjects:                    []string{issuer + "|alice", issuer + "|ops"},
		SecretsKEK:                       two,
		UpstreamAllowPrivate:             true,
		DiscoveryInterval:                15 * time.Minute,
		HealthInterval:                   time.Minute,
		KeyCache:                         30 * time.Second,
		DefaultRequestsPerMinute:         600,
		DefaultTokensPerMinute:           200000,
		PublicURL:                        &url.URL{Scheme: "https", Host: "gateway.example.com", Path: "/lux"},
		RequestsPerMinute:                1200,
		UnauthenticatedRequestsPerMinute: 0,
		TrustedProxies:                   []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8::/32")},
		MaxManifestBytes:                 1 << 20,
		MaxBodyBytes:                     8 << 20,
		UpstreamTimeout:                  2 * time.Minute,
		MeteringFlush:                    500 * time.Millisecond,
		EventsURL:                        "https://sink.example.com/events",
		EventsSecret:                     "whsec-9f8e7d",
		RequestLogExporter:               ExporterS3,
		S3Endpoint:                       "https://s3.eu-central-1.amazonaws.com",
		S3Region:                         "eu-central-1",
		S3Bucket:                         "lux-archive",
		S3AccessKey:                      "AKIDEXAMPLE",
		S3SecretKey:                      "wJalrXUtnFEMI",
		S3Prefix:                         "gateways/a/",
		TunnelEnabled:                    true,
		TunnelRegistryTTL:                45 * time.Second,
		TunnelForwardAddr:                "10.0.0.7:9001",
		TunnelForwardSecrets:             []string{secretNew, secretOld},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
	c, err = Load(env(map[string]string{
		"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek,
		"LUX_DB_URL":       "postgres://lux:secret@db.example.com:5432/lux?sslmode=require",
		"LUX_DB_MAX_CONNS": "20",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want = Config{
		PublicAddr: ":8080", InternalAddr: ":8081",
		DBURL: "postgres://lux:secret@db.example.com:5432/lux?sslmode=require", DBMaxConns: 20,
		OIDCIssuers: []string{issuer}, OIDCAudiences: []string{"lux"}, AuthorizerTimeout: 5 * time.Second,
		SecretsKEK: keyring(t), DiscoveryInterval: time.Hour, HealthInterval: 30 * time.Second,
		KeyCache: 10 * time.Second, MeteringFlush: time.Second,
		PublicURL: wantPublicURL(t), RequestsPerMinute: 600, UnauthenticatedRequestsPerMinute: 60,
		MaxManifestBytes: 65536, MaxBodyBytes: 64 << 20, UpstreamTimeout: 10 * time.Minute,
		RequestLogExporter: ExporterNone, S3Region: DefaultS3Region, S3Prefix: DefaultS3Prefix,
		TunnelRegistryTTL: 30 * time.Second,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReportsEveryProblemInOneSortedMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":   "nope",
		"LUX_INTERNAL_ADDR": "nope",
		"LUX_DB_URL":        "mysql://db.example.com/lux",
		"LUX_DB_MAX_CONNS":  "0",
		"LUX_OIDC_ISSUERS":  issuer,
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
	c, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR": "  ", "LUX_INTERNAL_ADDR": "", "LUX_MANIFEST_DIR": " ", "LUX_DB_URL": "\t", "LUX_DB_MAX_CONNS": " ",
		"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek, "LUX_OIDC_AUDIENCE": " ", "LUX_AUTHORIZER_URL": " ", "LUX_AUTHORIZER_TOKEN": "",
		"LUX_AUTHORIZER_TIMEOUT": "  ", "LUX_ADMIN_SUBJECTS": " , ",
		"LUX_UPSTREAM_ALLOW_PRIVATE": " ", "LUX_DISCOVERY_INTERVAL": "\t", "LUX_HEALTH_INTERVAL": "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr || c.ManifestDir != "" || c.DBURL != "" || c.DBMaxConns != DefaultDBMaxConns {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
	if !slices.Equal(c.OIDCAudiences, []string{DefaultOIDCAudience}) || c.AuthorizerURL != "" || c.AuthorizerTimeout != DefaultAuthorizerTimeout || c.AdminSubjects != nil {
		t.Fatalf("blank identity values did not fall back to defaults: %+v", c)
	}
	if c.UpstreamAllowPrivate || c.DiscoveryInterval != DefaultDiscoveryInterval || c.HealthInterval != DefaultHealthInterval {
		t.Fatalf("blank provider values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:9000", "LUX_INTERNAL_ADDR": "127.0.0.1:9000", "LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek}))
	if err == nil || !strings.Contains(err.Error(), "must differ from LUX_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:0", "LUX_INTERNAL_ADDR": "127.0.0.1:0", "LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek})); err != nil {
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
			_, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek, "LUX_DB_URL": tc.url}))
			if err == nil || !strings.Contains(err.Error(), "LUX_DB_URL "+tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("the message echoes the password:\n%s", err)
			}
		})
	}
	for _, ok := range []string{"postgres://db.example.com/lux", "postgresql://db.example.com/lux?sslmode=disable", "POSTGRES://db.example.com/lux"} {
		if _, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek, "LUX_DB_URL": ok})); err != nil {
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
			_, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek, "LUX_DB_URL": "postgres://db.example.com/lux", "LUX_DB_MAX_CONNS": tc.value}))
			if err == nil || !strings.Contains(err.Error(), "LUX_DB_MAX_CONNS "+tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	for _, edge := range []string{"1", "100"} {
		if _, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek, "LUX_DB_URL": "postgres://db.example.com/lux", "LUX_DB_MAX_CONNS": edge})); err != nil {
			t.Errorf("%s: %v", edge, err)
		}
	}
	c, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek, "LUX_DB_MAX_CONNS": "many"}))
	if err != nil || c.DBMaxConns != DefaultDBMaxConns {
		t.Fatalf("without LUX_DB_URL the pool size is not read: %+v, %v", c, err)
	}
}

// TestBootstrapRules is spec 035's LUX_BOOTSTRAP_DIR at load: a
// directory that exists, never beside the file mode's directory, and
// never without an admin subject to own what it applies; each problem
// names the variables.
func TestBootstrapRules(t *testing.T) {
	dir := t.TempDir()
	admin := issuer + "|alice"
	for _, tc := range []struct {
		name string
		env  map[string]string
		fail string // a fragment of the problem, or "" for a configuration that loads
	}{
		{"unset", map[string]string{"LUX_OIDC_ISSUERS": issuer}, ""},
		{"a directory with an admin subject", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_BOOTSTRAP_DIR": dir, "LUX_ADMIN_SUBJECTS": admin}, ""},
		{"no admin subject", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_BOOTSTRAP_DIR": dir},
			"LUX_BOOTSTRAP_DIR is set and LUX_ADMIN_SUBJECTS is empty"},
		{"beside the file mode", map[string]string{"LUX_MANIFEST_DIR": dir, "LUX_BOOTSTRAP_DIR": dir, "LUX_ADMIN_SUBJECTS": admin},
			"LUX_BOOTSTRAP_DIR and LUX_MANIFEST_DIR are both set"},
		{"a directory that does not exist", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_BOOTSTRAP_DIR": filepath.Join(dir, "absent"), "LUX_ADMIN_SUBJECTS": admin},
			"LUX_BOOTSTRAP_DIR "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(withKEK(tc.env)))
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) {
					t.Fatalf("Load() = %v, want a problem with %q", err, tc.fail)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if c.BootstrapDir != tc.env["LUX_BOOTSTRAP_DIR"] {
				t.Fatalf("BootstrapDir = %q", c.BootstrapDir)
			}
		})
	}
}
