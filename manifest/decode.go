// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"encoding/json"
	"mime"
	"reflect"
	"strconv"
	"strings"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Hint is what the surface decoding a body already knows: the API fills
// it from the route, so PUT /v1/keys/run-42 means lux.latere.ai/v1beta1,
// Key, run-42, and a body of spec alone is a complete manifest; the file
// mode and the lux command leave it empty and the envelope is required.
type Hint struct {
	APIVersion string
	Kind       string
	Name       string
}

// The content types Decode reads.
const (
	MediaJSON  = "application/json"
	MediaYAML  = "application/yaml"
	MediaXYAML = "application/x-yaml"
	MediaText  = "text/yaml"
)

// Decode reads one manifest of any kind from a YAML or JSON body. A YAML
// body is parsed into a generic tree, held to MaxScalarBytes and MaxDepth,
// and checked against the kind's type by the json tags that encoding/json
// then decodes it with, DisallowUnknownFields set, so an unknown field is
// refused with one path spelling whichever format the body came in, and
// the YAML and JSON forms of one manifest decode to equal objects.
//
// apiVersion and kind are checked before anything else, version first.
// status is ignored, never an error, so a caller may GET, edit, and PUT
// what it read. The write-only fields are decoded into the members the
// encoders skip and are read through their accessors. Every refusal is an
// *Error.
func Decode(body []byte, contentType string, hint Hint) (v1.Object, error) {
	tree, err := parse(body, contentType)
	if err != nil {
		return nil, err
	}
	kind, err := envelope(tree, hint)
	if err != nil {
		return nil, err
	}
	delete(tree, "apiVersion")
	delete(tree, "kind")
	delete(tree, "status")

	obj := newObject(kind)
	w := &schemaWalk{writeOnly: map[string]string{}}
	if err := w.object(tree, reflect.TypeOf(obj).Elem(), ""); err != nil {
		return nil, err
	}
	if err := checkName(tree, hint); err != nil {
		return nil, err
	}
	data, err := json.Marshal(tree)
	if err != nil {
		return nil, refuse(CodeMalformedBody, "the tree does not encode: "+err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(obj); err != nil {
		return nil, refuse(CodeInvalidField, "the body does not fit the schema: "+err.Error())
	}
	setWriteOnly(obj, w.writeOnly)
	if hint.Name != "" {
		if meta := metaOf(obj); meta.Name == "" {
			meta.Name = hint.Name
		}
	}
	return obj, nil
}

// parse picks the parser by content type. A body that begins with { under
// a YAML type is JSON.
func parse(body []byte, contentType string) (map[string]any, error) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, refuse(CodeUnsupportedMediaType, "content type "+strconv.Quote(contentType)+": "+err.Error())
	}
	switch strings.ToLower(mediaType) {
	case MediaJSON:
		return parseJSON(body)
	case MediaYAML, MediaXYAML, MediaText:
		if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{")) {
			return parseJSON(body)
		}
		return parseYAML(body)
	default:
		return nil, refuse(CodeUnsupportedMediaType, "content type "+strconv.Quote(mediaType)+" is not JSON or YAML")
	}
}

// envelope settles apiVersion and kind from the body and the hint, version
// first, and returns the kind. A field present in the body and different
// from the hint is refused; one absent takes the hint's; without a hint
// both are required.
func envelope(tree map[string]any, hint Hint) (string, error) {
	version, err := envelopeField(tree, "apiVersion", hint.APIVersion, CodeUnsupportedVersion)
	if err != nil {
		return "", err
	}
	if version != v1.APIVersion {
		return "", refuse(CodeUnsupportedVersion, "apiVersion "+strconv.Quote(version)+" is not "+v1.APIVersion, "apiVersion")
	}
	kind, err := envelopeField(tree, "kind", hint.Kind, CodeUnsupportedKind)
	if err != nil {
		return "", err
	}
	if !v1.KnownKind(kind) {
		return "", refuse(CodeUnsupportedKind, "kind "+strconv.Quote(kind)+" is not Provider, Model, Key, or Budget", "kind")
	}
	return kind, nil
}

// envelopeField reads one of the two envelope fields against its hint.
func envelopeField(tree map[string]any, name, hinted string, code Code) (string, error) {
	raw, present := tree[name]
	if !present || raw == nil {
		if hinted == "" {
			return "", refuse(CodeMissingField, name+" is required without a route to supply it", name)
		}
		return hinted, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", refuse(code, name+" is not a string", name)
	}
	if hinted != "" && s != hinted {
		return "", refuse(code, "the body says "+name+" "+strconv.Quote(s)+", the route "+strconv.Quote(hinted), name)
	}
	return s, nil
}

// checkName refuses a metadata.name that disagrees with the hint's.
func checkName(tree map[string]any, hint Hint) error {
	if hint.Name == "" {
		return nil
	}
	meta, ok := tree["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	name, ok := meta["name"].(string)
	if ok && name != hint.Name {
		return refuse(CodeInvalidField, "the body names "+strconv.Quote(name)+", the route "+strconv.Quote(hint.Name), "metadata.name")
	}
	return nil
}

// newObject is the zero object of a kind.
func newObject(kind string) v1.Object {
	switch kind {
	case v1.KindProvider:
		return &v1.Provider{}
	case v1.KindModel:
		return &v1.Model{}
	case v1.KindKey:
		return &v1.Key{}
	case v1.KindBudget:
		return &v1.Budget{}
	default:
		panic("manifest: newObject of unknown kind " + kind)
	}
}

// metaOf returns the object's metadata to fill in place.
func metaOf(obj v1.Object) *v1.ObjectMeta {
	switch o := obj.(type) {
	case *v1.Provider:
		return &o.Metadata
	case *v1.Model:
		return &o.Metadata
	case *v1.Key:
		return &o.Metadata
	case *v1.Budget:
		return &o.Metadata
	default:
		panic("manifest: metaOf of unknown object")
	}
}

// setWriteOnly hands the values the schema walk took out of the tree to
// the accessors of the three fields that carry them.
func setWriteOnly(obj v1.Object, values map[string]string) {
	switch o := obj.(type) {
	case *v1.Provider:
		if v, ok := values["spec.credential.value"]; ok {
			if o.Spec.Credential == nil {
				o.Spec.Credential = &v1.Credential{}
			}
			o.Spec.Credential.SetValue(v)
		}
	case *v1.Key:
		if v, ok := values["spec.value"]; ok {
			o.Spec.SetValue(v)
		}
		if v, ok := values["spec.valueSHA256"]; ok {
			o.Spec.SetValueSHA256(v)
		}
	}
}
