// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strconv"
	"strings"
)

// precondition is what If-Match and If-None-Match asked: exists is
// If-Match: *, version is If-Match: "<n>", and free is If-None-Match: *.
// The zero value is no precondition, the read-modify-write case.
type precondition struct {
	exists  bool
	version int64
	free    bool
}

// none reports no precondition.
func (p precondition) none() bool { return !p.exists && p.version == 0 && !p.free }

// parsePrecondition reads the two headers. The only forms accepted are
// * and one quoted decimal integer on If-Match, and * on If-None-Match;
// a weak validator, a list, an unquoted number, or both headers
// together is invalid_field at the header, never a silent match.
func parsePrecondition(h http.Header) (precondition, *Error) {
	var p precondition
	match, noneMatch := h.Get("If-Match"), h.Get("If-None-Match")
	if match != "" && noneMatch != "" {
		return p, refuse(CodeInvalidField, "If-Match and If-None-Match were both sent, and one precondition is accepted", "If-Match", "If-None-Match")
	}
	if len(h.Values("If-Match")) > 1 || len(h.Values("If-None-Match")) > 1 {
		return p, refuse(CodeInvalidField, "the header was sent more than once", "If-Match")
	}
	switch {
	case match == "*":
		p.exists = true
	case match != "":
		v, ok := quotedVersion(match)
		if !ok {
			return p, refuse(CodeInvalidField, "If-Match "+match+" is not * or one quoted integer such as \"7\"", "If-Match")
		}
		p.version = v
	case noneMatch == "*":
		p.free = true
	case noneMatch != "":
		return p, refuse(CodeInvalidField, "If-None-Match "+noneMatch+" is not *, the one form accepted", "If-None-Match")
	}
	return p, nil
}

// quotedVersion reads "<n>" for a decimal n above zero.
func quotedVersion(s string) (int64, bool) {
	if len(s) < 3 || !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return 0, false
	}
	n, err := strconv.ParseInt(s[1:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// check holds the loaded object, or none, to the precondition at the
// act: a free name under If-Match is not_found, a taken one under
// If-None-Match is already_exists, and another version is conflict.
func (p precondition) check(exists bool, version int64) *Error {
	switch {
	case p.free && exists:
		return refuse(CodeAlreadyExists, "If-None-Match: * was sent and an object of that name exists at version "+strconv.FormatInt(version, 10))
	case (p.exists || p.version > 0) && !exists:
		return refuse(CodeNotFound, "If-Match was sent and no object of that name exists")
	case p.version > 0 && version != p.version:
		return refuse(CodeConflict, "If-Match \""+strconv.FormatInt(p.version, 10)+"\" was sent and the object is at version "+strconv.FormatInt(version, 10))
	}
	return nil
}
