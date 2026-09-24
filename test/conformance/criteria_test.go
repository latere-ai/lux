// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// covered maps every test an acceptance row of specs 003, 004, and 011
// names to the case that proves the same criterion over HTTP. A test
// that proves something no request can reach is in unreachable below
// with why, so a row of those three specs is either a case or a stated
// reason and never an omission.
var covered = map[string]string{
	// 003
	"TestCorpusCoversEveryDecodeCode":   "case003RefusedCorpus",
	"TestCredentialUpdate":              "case011SecretsNeverInResponses",
	"TestCredentialValueNeverEncodes":   "case011SecretsNeverInResponses",
	"TestDecodeRefusals":                "case003RefusedCorpus",
	"TestDecodeYAMLAndJSONAgree":        "case011BodiesAndTypes",
	"TestDefaultsFillOnlyAbsentFields":  "case003DefaultsAreVisible",
	"TestDialectDefaults":               "case003DefaultsAreVisible",
	"TestExclusiveMissingAndDuplicates": "case003RefusedCorpus",
	"TestFieldSyntax":                   "case003RefusedCorpus",
	"TestGlob":                          "case003RefusedCorpus",
	"TestGoldenCorpus":                  "case003AcceptedCorpus",
	"TestHashSuppliedValueSchema":       "case003RefusedCorpus",
	"TestHintDisagreementIsRefused":     "case011ApplyWithoutTheEnvelope",
	"TestHintFillsTheEnvelope":          "case011ApplyWithoutTheEnvelope",
	"TestImmutableFields":               "case011ApplyIsCreateThenUpdate",
	"TestLookupErrors":                  "case003RefusedCorpus",
	"TestMoney":                         "case003RefusedCorpus",
	"TestNameGeneration":                "case003AcceptedCorpus",
	"TestNamesNeverLookLikeIds":         "case003RefusedCorpus",
	"TestSelectorsRecordTheirMatches":   "case011GrammarPerKind",
	"TestSuppliedValueSchema":           "case003RefusedCorpus",
	"TestTheExamplesResolve":            "case003AcceptedCorpus",
	"TestUnknownFieldNamesThePath":      "case003UnknownField",
	"TestUpstreamHostRule":              "case003RefusedCorpus",
	"TestValueFromIsFileModeOnly":       "case003RefusedCorpus",
	"TestYAMLLimits":                    "case003RefusedCorpus",
	// 004
	"TestAnthropicVersionInjected":                  "case004TranslationLoss",
	"TestCallerCredentialsNeverForwarded":           "case004CallerCredentialsNeverForwarded",
	"TestCountTokensEmulation":                      "case004CountTokens",
	"TestDataPlaneServesWhileAuthorizerIsDown":      "case006AuthorizerUnavailable",
	"TestDialectBridging":                           "case004DialectBridging",
	"TestDoorsTakeKeysOnly":                         "case006TokenOnDoorIsUnauthenticated",
	"TestErrorEnvelopePerDialect":                   "case004ErrorEnvelopePerDialect",
	"TestIncludeUsageInjected":                      "case004Streaming",
	"TestInvalidRequestBodies":                      "case004RouteTable",
	"TestKeyExtractionOrder":                        "case004CredentialForms",
	"TestModelNameRewrite":                          "case004ModelNameRewrite",
	"TestModelsListIsTheKeysView":                   "case004ModelsListIsTheKeysView",
	"TestModelsListShapes":                          "case004ModelsListIsTheKeysView",
	"TestNoHTMLIsEverServed":                        "case004ErrorEnvelopePerDialect",
	"TestNoLossNoHeader":                            "case004TranslationLoss",
	"TestOpaqueRoutes":                              "case004OpaqueRoute",
	"TestPassthroughForwardsWhatTheCodecCannotRead": "case004SameDialectSameBytes",
	"TestRequestIDOnEveryResponse":                  "case011RequestIdOnEveryResponse",
	"TestRouteTable":                                "case004RouteTable",
	"TestSameDialectSameBytes":                      "case004SameDialectSameBytes",
	"TestStreamingPassthrough":                      "case004Streaming",
	"TestStreamingTranslation":                      "case004Streaming",
	"TestStreamUsageIsTheLastValue":                 "case004Streaming",
	"TestSuppliedValueOpensTheDoor":                 "case007SuppliedValue",
	"TestTranslationReportsLoss":                    "case004TranslationLoss",
	"TestUpstreamBodyIsDetailOnly":                  "case004UpstreamError",
	"TestUpstreamStatusMapping":                     "case004UpstreamError",
	// 011
	"TestAddressByIdOrName":                         "case011AddressByIdOrName",
	"TestApplyIsByNameOnly":                         "case011AddressByIdOrName",
	"TestApplyIsCreateThenUpdate":                   "case011ApplyIsCreateThenUpdate",
	"TestApplyWithoutTheEnvelope":                   "case011ApplyWithoutTheEnvelope",
	"TestBodiesAndTypes":                            "case011BodiesAndTypes",
	"TestBudgetInUse":                               "case011BudgetInUse",
	"TestEnvelope":                                  "case011Envelope",
	"TestFileModeIsReadOnly":                        "case011ReadOnlyInFileMode",
	"TestFileModeMountsOnTheInternalListener":       "case011ReadOnlyInFileMode",
	"TestGrammarPerKind":                            "case011GrammarPerKind",
	"TestListSelectors":                             "case011Pagination",
	"TestMalformedBody":                             "case003RefusedCorpus",
	"TestModelNamesWithSlashes":                     "case011ModelNamesWithSlashes",
	"TestNameOnApply":                               "case011ApplyWithoutTheEnvelope",
	"TestNoCORS":                                    "case011NoCORS",
	"TestNoPatch":                                   "case011Envelope",
	"TestOpenAPIServedMatchesCommitted":             "case011OpenAPIValidatesEveryResponse",
	"TestPagination":                                "case011Pagination",
	"TestPreconditions":                             "case011Preconditions",
	"TestRateLimits":                                "case011RateLimitHeaders",
	"TestRequestIdOnEveryResponse":                  "case011RequestIdOnEveryResponse",
	"TestRequestsSource":                            "case009RefusedRequestHasARecord",
	"TestRotate":                                    "case011Rotate",
	"TestRevocationPropagates":                      "case007RotateInvalidatesTheOldValue",
	"TestSecretsNeverInResponses":                   "case011SecretsNeverInResponses",
	"TestSelf":                                      "case011Self",
	"TestSuppliedValueIsNotEchoed":                  "case007SuppliedValue",
	"TestUnknownQueryParameter":                     "case011Pagination",
	"TestUsageEnvelopes":                            "case009UsageAggregates",
	"TestUsageFilterNarrows":                        "case009UsageAggregates",
	"TestUsageQueryValidation":                      "case009UsageAggregates",
	"TestUsageRoutesResolveNamesAndIntersectFilter": "case009UsageAggregates",
	"TestWellKnown":                                 "case011WellKnown",
	"TestXRequestIdIsEchoedOnly":                    "case011RequestIdOnEveryResponse",
}

// unreachable names the tests of the three specs that prove something no
// HTTP request to an arbitrary server can, each with why.
var unreachable = map[string]string{
	// 003
	"TestLimits":                     "the ceilings are an authorizer's limits, which the suite cannot set on an operator's authorizer",
	"TestLookupFailurePassesThrough": "a store failure inside Resolve cannot be caused from outside",
	"TestManifestImports":            "an import graph is read from the source, not over HTTP",
	"TestNewID":                      "the id generator is proved in process; the suite reads ids as opaque",
	"TestRootPackagesDialNothing":    "an import graph is read from the source, not over HTTP",
	"TestWindows":                    "the window arithmetic is proved in process; the suite reads resetsAt as a value",
	// 004
	"TestBodyLimit":                       "the limit is the operator's LUX_MAX_BODY_BYTES, up to 1Gi, which a suite cannot send",
	"TestClientDisconnectCancelsUpstream": "the cancellation is measured in process within 100 ms of the disconnect",
	"TestHotPathDialsNoWebhook":           "the stub issuer's and authorizer's recorders are read across a process boundary by spec 015's tier",
	"TestNoRetryAfterFirstByte":           "a stream that fails after its first byte is spec 015's fail-stream-mid stub, not built",
	"TestRefusalOrder":                    "two refusals holding at once are staged in process against fakes",
	"TestStreamErrorFramePerDoor":         "a stream that fails after its first byte is spec 015's fail-stream-mid stub, not built",
	// 011
	"TestClientAddressRule":                            "the client address rule reads LUX_TRUSTED_PROXIES, the operator's",
	"TestConcurrentApplyRetries":                       "a version conflict between a read and a write is staged in process",
	"TestDenyReasonStaysInDetail":                      "a deny is the operator's authorizer's to give",
	"TestErrorTable":                                   "the table is read from the specs and the handler sources",
	"TestLimitUnauthenticatedCountsRefusedCredentials": "the per-address bucket is LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE, the operator's",
	"TestOpenAPIIsCurrent":                             "the committed api/openapi.yaml is a file of this tree",
	"TestRouteTableActions":                            "the action asked of the authorizer is seen by the authorizer, not the caller",
	"TestStoreErrorsMap":                               "a store failure cannot be caused from outside",
	"TestTrustedProxies":                               "the client address rule reads LUX_TRUSTED_PROXIES, the operator's",
	"TestUnauthenticatedBucketSpansBothPlanes":         "the per-address bucket is LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE, the operator's",
}

// caseName is the shape of a case's name: case, the spec's number, a
// capitalized name.
var caseName = regexp.MustCompile(`^case(\d{3})[A-Z][A-Za-z0-9]*$`)

// specTests reads the test names of one spec's acceptance table.
func specTests(t *testing.T, dir, prefix string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("spec %s: %v %v", prefix, matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	in := false
	var names []string
	testName := regexp.MustCompile(`Test[A-Za-z0-9_]+`)
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "## Acceptance criteria"):
			in = true
			continue
		case strings.HasPrefix(line, "## "):
			in = false
		}
		if in && strings.HasPrefix(line, "| ") {
			names = append(names, testName.FindAllString(line, -1)...)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// TestEveryCriterionHasACase: every test an acceptance row of specs 003,
// 004, and 011 names is covered by a case or stated unreachable, every
// case named exists, and every case's number names a spec in the deck.
func TestEveryCriterionHasACase(t *testing.T) {
	specs := filepath.Join(moduleRoot(t), "specs")
	names := map[string]bool{}
	for _, tc := range cases {
		if names[tc.name] {
			t.Errorf("%s is in the table twice", tc.name)
		}
		names[tc.name] = true
		m := caseName.FindStringSubmatch(tc.name)
		if m == nil {
			t.Errorf("%s is not case<NNN><Name>", tc.name)
			continue
		}
		// A spec keeps its number when it is archived, so a case names a
		// live spec or an archived one.
		live, _ := filepath.Glob(filepath.Join(specs, m[1]+"-*.md"))
		archived, _ := filepath.Glob(filepath.Join(specs, ".archive", m[1]+"-*.md"))
		if len(live)+len(archived) != 1 {
			t.Errorf("%s names spec %s, which is not in the deck", tc.name, m[1])
		}
		if itoa(tc.spec) != strings.TrimLeft(m[1], "0") {
			t.Errorf("%s is registered under spec %d", tc.name, tc.spec)
		}
	}
	names["case007SuiteMintsItsKey"] = true
	for _, prefix := range []string{"003", "004", "011"} {
		for _, test := range specTests(t, specs, prefix) {
			c, isCovered := covered[test]
			_, isUnreachable := unreachable[test]
			switch {
			case isCovered && isUnreachable:
				t.Errorf("%s of spec %s is both covered and unreachable", test, prefix)
			case isCovered && !names[c]:
				t.Errorf("%s of spec %s is covered by %s, which is not a case", test, prefix, c)
			case !isCovered && !isUnreachable:
				t.Errorf("%s of spec %s is neither covered by a case nor stated unreachable", test, prefix)
			}
		}
	}
	for test := range covered {
		if _, dup := unreachable[test]; dup {
			t.Errorf("%s is in both tables", test)
		}
	}
}
