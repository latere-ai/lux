// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

var fenceName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func (c *call) keyFence(ctx context.Context, name string) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	if !fenceName.MatchString(name) || hasKindPrefix(name) {
		return refuse(CodeInvalidField, "a fence requires a Key name, not an id", "name")
	}
	if len(c.r.Header.Values("If-Match"))+len(c.r.Header.Values("If-None-Match")) != 0 {
		return refuse(CodeInvalidField, "Key version preconditions do not apply to immutable fences", "If-Match", "If-None-Match")
	}
	if c.r.Method == http.MethodGet {
		if !c.h.fileMode() {
			if _, err := c.authorize(ctx, authorizer.ActionKeyFenceRead, authorizer.KeyFenceRead(name)); err != nil {
				return err
			}
		}
		result, err := c.h.o.Store.KeyFences().Get(ctx, name)
		if err != nil {
			return mapError(err)
		}
		return c.writeJSON(http.StatusOK, result, 0)
	}
	contentType, _, err := mime.ParseMediaType(c.r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return refuse(CodeUnsupportedMediaType, "a fence assertion requires application/json")
	}
	body, bodyErr := c.readBody()
	if bodyErr != nil {
		return bodyErr
	}
	input, err := decodeFence(body)
	if err != nil {
		return refuse(CodeInvalidRequest, err.Error())
	}
	input.Name = name
	issuer, subject, ok := authz.SplitSubject(input.Owner)
	if !ok || issuer == "" || subject == "" || len(input.Owner) > 2048 || strings.TrimSpace(input.Owner) != input.Owner {
		return refuse(CodeInvalidField, "owner must be a rendered issuer|subject", "owner")
	}
	if _, err := c.authorize(ctx, authorizer.ActionKeyFence, authorizer.KeyFenceInstall(name, input.Owner, input.Labels)); err != nil {
		return err
	}
	var result store.KeyFence
	err = c.h.o.Store.Transact(ctx, func(tx store.Store) error {
		var inserted bool
		var err error
		result, inserted, err = tx.KeyFences().Put(ctx, input)
		if err != nil || !inserted {
			return err
		}
		return events.AppendRef(ctx, tx.Journal(), events.Event{
			ID: c.h.o.NewID(v1.PrefixEvent), Type: events.KeyFenced, At: result.CreatedAt, Subject: c.caller.Subject, Reason: events.ReasonRequest, RequestID: c.id,
		}, events.ObjectRef{Kind: authorizer.KindKeyFence, ID: "key-fence/" + name, Name: name, Owner: result.Owner, Labels: result.Labels})
	})
	if err != nil {
		return mapError(err)
	}
	return c.writeJSON(http.StatusOK, result, 0)
}

// jsonMembers retains raw values while rejecting duplicate names and trailing
// documents. Parsing labels through the same function rejects nested duplicates.
func jsonMembers(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	result := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("expected a JSON field name")
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("duplicate JSON field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		result[name] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected content after JSON object")
	}
	return result, nil
}

func decodeFence(body []byte) (store.KeyFence, error) {
	input := store.KeyFence{Labels: map[string]string{}}
	fields, err := jsonMembers(body)
	if err != nil {
		return input, err
	}
	for name, raw := range fields {
		switch name {
		case "owner":
			if err := json.Unmarshal(raw, &input.Owner); err != nil {
				return input, errors.New("owner must be a string")
			}
		case "labels":
			labels, err := jsonMembers(raw)
			if err != nil {
				return input, err
			}
			for key, raw := range labels {
				var value string
				if len(raw) == 0 || raw[0] != '"' {
					return input, errors.New("labels must contain strings")
				}
				if err := json.Unmarshal(raw, &value); err != nil {
					return input, err
				}
				input.Labels[key] = value
			}
		default:
			return input, errors.New("unknown fence assertion field")
		}
	}
	return input, nil
}
