// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package authorizer mounts latere.ai/x/pkg/authz/stub, the stub of the
// contract spec 006 adopts, with Lux's vocabulary and nothing else: a
// rule names an object as an operator does, Kind/name, rather than by a
// ULID a test cannot know in advance, and the two lux-stubs flags set the
// rule table and the outage through the package's own methods. The
// decision, the recording, and the outage modes are the package's and
// are tested there.
package authorizer

import (
	"errors"
	"strconv"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
)

// Name renders a resource the way a rule names it: Kind/name, from the
// name member spec 006's resource shapes carry; "" for a resource with
// no name, which then matches by id or by * alone.
func Name(r authz.Resource) string {
	name := r.String("name")
	if name == "" {
		return ""
	}
	return r.Kind + "/" + name
}

// NewHandler builds the stub for a binary serving its own address, with
// Name as its resource naming; opts come after it.
func NewHandler(opts ...stub.Option) *stub.Server {
	return stub.NewHandler(append([]stub.Option{stub.WithResourceName(Name)}, opts...)...)
}

// New starts the stub on a loopback listener for the test.
func New(t testing.TB, opts ...stub.Option) *stub.Server {
	t.Helper()
	return stub.New(t, append([]stub.Option{stub.WithResourceName(Name)}, opts...)...)
}

// Deny adds a rule denying action for every subject and resource, what
// lux-stubs -authorizer-deny <action> sets; "" denies nothing.
func Deny(s *stub.Server, action string) {
	if action == "" {
		return
	}
	s.Deny(stub.Rule{Action: action}, "denied by lux-stubs -authorizer-deny "+action)
}

// Fail puts the stub into the outage mode named, what lux-stubs
// -authorizer-fail <mode> sets: a status such as 503, malformed or
// no-allow for a 200 that is no decision, hang for no answer, and "" for
// none. Anything else is an error naming the modes.
func Fail(s *stub.Server, mode string) error {
	switch mode {
	case "":
		return nil
	case string(stub.BodyMalformed), string(stub.BodyNoAllow):
		s.FailBody(stub.Body(mode))
		return nil
	case "hang":
		s.Hang()
		return nil
	}
	status, err := strconv.Atoi(mode)
	if err != nil || status < 100 || status > 599 || status == 200 {
		return errors.New("-authorizer-fail " + strconv.Quote(mode) + " is not an HTTP status other than 200, malformed, no-allow, or hang")
	}
	s.Fail(status)
	return nil
}
