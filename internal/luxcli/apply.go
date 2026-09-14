// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

type applyOptions struct {
	files       multi
	credentials multi
	ifMatch     string
}

func applyFlags(a *app, fs *flag.FlagSet) func([]string) error {
	o := &applyOptions{}
	fs.Var(&o.files, "f", "a manifest file; repeatable, applied in order")
	fs.Var(&o.credentials, "credential-from-env", "NAME or <provider>=NAME: send the variable's value as the Provider's credential")
	fs.StringVar(&o.ifMatch, "if-match", "", "apply only at this version, or * to update an existing object; one document")
	return func(args []string) error { return runApply(a, o, args) }
}

// document is one manifest on its way to the server: where it came from,
// the object Decode read, and the bytes to send.
type document struct {
	file        string
	index       int
	obj         v1.Object
	raw         []byte
	contentType string
	plural      string
	name        string
}

func runApply(a *app, o *applyOptions, args []string) error {
	if len(args) > 0 {
		return &usageError{cmd: byName("apply"), msg: "lux apply takes no argument; name the files with -f."}
	}
	if len(o.files) == 0 {
		return &usageError{cmd: byName("apply"), msg: "Name at least one manifest file with -f."}
	}
	if err := checkIfMatch(o.ifMatch); err != nil {
		return err
	}
	var docs []*document
	for _, file := range o.files {
		read, err := readDocuments(file)
		if err != nil {
			return err
		}
		docs = append(docs, read...)
	}
	if len(docs) == 0 {
		return &usageError{msg: "The files hold no manifest."}
	}
	if o.ifMatch != "" && len(docs) > 1 {
		return &usageError{msg: "-if-match names one version, and the files hold " + strconv.Itoa(len(docs)) + " manifests; apply them one at a time."}
	}
	if err := a.injectCredentials(docs, o.credentials); err != nil {
		return err
	}
	c, err := a.controlClient()
	if err != nil {
		return err
	}
	for _, d := range docs {
		resp, err := c.Apply(a.ctx, d.plural, d.name, d.raw, d.contentType, o.ifMatch)
		if err != nil {
			return classify(err)
		}
		if err := a.print(resp.Body); err != nil {
			return err
		}
	}
	return nil
}

// readDocuments reads one file, splits it into YAML documents, and
// decodes each with manifest.Decode, the server's own decoder, so a
// document the server would refuse is refused here with the same code
// and nothing is sent. A document is sent as the bytes it was written
// in, JSON or YAML, unless a credential is injected.
func readDocuments(file string) ([]*document, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, &usageError{msg: "The file " + quote(file) + " cannot be read."}
	}
	var docs []*document
	for i, raw := range splitDocuments(data) {
		contentType := manifest.MediaYAML
		if bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("{")) {
			contentType = manifest.MediaJSON
		}
		obj, err := manifest.Decode(raw, contentType, manifest.Hint{})
		if err != nil {
			return nil, decodeError(file, i+1, err)
		}
		name := obj.Name()
		if name == "" {
			return nil, &usageError{msg: where(file, i+1) + "The manifest has no metadata.name, and apply is by name."}
		}
		docs = append(docs, &document{file: file, index: i + 1, obj: obj, raw: raw, contentType: contentType, plural: kinds[strings.ToLower(obj.Kind())], name: name})
	}
	return docs, nil
}

// decodeError is a refusal from Decode as exit 2: the code's fixed
// sentence after the file and the document, with the developer detail
// left to -v, where a server refusal would have put it.
func decodeError(file string, index int, err error) error {
	if me, ok := errors.AsType[*manifest.Error](err); ok {
		msg := me.Message
		if msg == "" {
			msg = messageRefused
		}
		return &usageError{msg: where(file, index) + msg + detailSuffix(me)}
	}
	return &usageError{msg: where(file, index) + "The manifest does not decode."}
}

// detailSuffix names the paths of a decode refusal, since a person
// fixing a file needs the field and there is no -v run to ask for it.
func detailSuffix(e *manifest.Error) string {
	if len(e.Paths) == 0 {
		return ""
	}
	return " (" + strings.Join(e.Paths, ", ") + ")"
}

func where(file string, index int) string {
	return file + ": document " + strconv.Itoa(index) + ": "
}

// splitDocuments cuts a file at every line that is a YAML document
// marker, `---` alone or followed by a space, and drops documents that
// hold only blank lines and comments. A JSON file has no marker and is
// one document.
func splitDocuments(data []byte) [][]byte {
	var docs [][]byte
	var cur []string
	flush := func() {
		text := strings.TrimRight(strings.Join(cur, "\n"), "\n")
		if !blank(text) {
			docs = append(docs, []byte(text+"\n"))
		}
		cur = nil
	}
	for line := range strings.SplitSeq(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if line == "---" || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "---\t") {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return docs
}

// blank reports a text of blank lines and comments alone.
func blank(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			return false
		}
	}
	return true
}

// injectCredentials applies every -credential-from-env to its Provider:
// the bare form to the one Provider among the documents, the named form
// to the Provider of that name. Each rule of spec 014 is a usage error
// here, before any request: two Providers and no name, no Provider, a
// name that is not a Provider among the documents, a document that
// already carries a credential source, and a variable that is unset or
// empty.
func (a *app) injectCredentials(docs []*document, flags multi) error {
	for _, f := range flags {
		target, variable, named := strings.Cut(f, "=")
		if !named {
			variable, target = f, ""
		}
		if variable == "" {
			return &usageError{msg: "-credential-from-env names a variable, and " + quote(f) + " names none."}
		}
		var providers []*document
		for _, d := range docs {
			if _, ok := d.obj.(*v1.Provider); ok && (!named || d.name == target) {
				providers = append(providers, d)
			}
		}
		switch {
		case len(providers) == 0 && named:
			return &usageError{msg: "-credential-from-env names " + quote(target) + ", and no Provider of that name is among the documents."}
		case len(providers) == 0:
			return &usageError{msg: "-credential-from-env needs a Provider among the documents, and there is none."}
		case len(providers) > 1:
			names := make([]string, 0, len(providers))
			for _, p := range providers {
				names = append(names, p.name)
			}
			return &usageError{msg: "-credential-from-env " + quote(variable) + " needs a Provider name, since the documents hold " + strings.Join(names, " and ") + "; write <provider>=" + variable + "."}
		}
		d := providers[0]
		p, ok := d.obj.(*v1.Provider)
		if !ok {
			return errors.New("a document filtered as a Provider is not one")
		}
		if p.Spec.Credential != nil {
			if _, has := p.Spec.Credential.Value(); has || p.Spec.Credential.ValueFrom != nil {
				return &usageError{msg: where(d.file, d.index) + "The Provider " + quote(d.name) + " already carries a credential; drop it from the file or drop -credential-from-env."}
			}
		}
		value := a.o.Getenv(variable)
		if value == "" {
			return &usageError{msg: "The variable " + variable + " is unset or empty."}
		}
		body, err := withCredential(p, value)
		if err != nil {
			return err
		}
		d.raw, d.contentType = body, manifest.MediaJSON
	}
	return nil
}

// withCredential encodes the Provider as JSON with the value put into
// spec.credential.value, the one member the encoders skip, so the body
// carries what the decoded object cannot.
func withCredential(p *v1.Provider, value string) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*object)
	if !ok {
		return nil, errors.New("the encoded Provider is not an object")
	}
	spec, ok := o.m["spec"].(*object)
	if !ok {
		spec = newObject()
		o.set("spec", spec)
	}
	cred, ok := spec.m["credential"].(*object)
	if !ok {
		cred = newObject()
		spec.set("credential", cred)
	}
	cred.set("value", value)
	o.del("status")
	return json.Marshal(o)
}

// manifestJSON is a locally built object as the manifest a person would
// write: the envelope, metadata, and spec, without the status the
// server owns.
func manifestJSON(obj v1.Object) (*object, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(data)
	if err != nil {
		return nil, fmt.Errorf("the manifest does not decode: %w", err)
	}
	o, ok := v.(*object)
	if !ok {
		return nil, errors.New("the encoded manifest is not an object")
	}
	o.del("status")
	return o, nil
}
