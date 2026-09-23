// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// Options is what one run reads the directory under and writes with.
// Dir, Store, and Owner are required; the rest have the defaults the
// zero value gives.
type Options struct {
	// Dir is LUX_BOOTSTRAP_DIR.
	Dir string
	// Store is the store serve constructed, which the run reads every
	// object from by name and writes to.
	Store store.Store
	// Owner is the rendered <iss>|<sub> subject every created object is
	// owned by, the first entry of LUX_ADMIN_SUBJECTS. An object that
	// already exists keeps its owner, as an update through /v1 does.
	Owner string
	// Keys is LUX_SECRETS_KEK: it seals a Provider's credential, and
	// opens the stored one to tell whether the document's differs. A
	// document that carries a credential with Keys nil is refused.
	Keys *secrets.Keyring
	// Getenv reads the variables valueFrom.env names; nil is os.Getenv.
	Getenv func(string) string
	// Defaults, AllowPrivateUpstreams, TunnelEnabled, and PublicURL are
	// Resolve's options from the configuration, as the API sets them.
	Defaults              manifest.Defaults
	AllowPrivateUpstreams bool
	TunnelEnabled         bool
	PublicURL             *url.URL
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints a prefixed ULID for a created object and an event;
	// nil uses v1.NewID.
	NewID func(prefix string) string
}

func (o Options) getenv(name string) string {
	if o.Getenv != nil {
		return o.Getenv(name)
	}
	return os.Getenv(name)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) newID(prefix string) string {
	if o.NewID != nil {
		return o.NewID(prefix)
	}
	return v1.NewID(prefix, o.now(), nil)
}

// valid refuses options no directory could be applied under: a missing
// directory or store, and an owner that is not a rendered subject, which
// the owner policy could never match.
func (o Options) valid() error {
	if o.Dir == "" {
		return errors.New("bootstrap: no directory")
	}
	if o.Store == nil {
		return errors.New("bootstrap: no store")
	}
	issuer, sub, ok := authz.SplitSubject(o.Owner)
	if !ok || issuer == "" || sub == "" || strings.TrimSpace(o.Owner) != o.Owner {
		return fmt.Errorf("bootstrap: owner %q is not a rendered issuer|subject", o.Owner)
	}
	return nil
}

// Action is what an apply did to one object, or what a dry run found
// it would do.
type Action string

// The three actions of spec 035's idempotence table.
const (
	Created   Action = "created"
	Updated   Action = "updated"
	Unchanged Action = "unchanged"
)

// Outcome is one document's action.
type Outcome struct {
	// File is the document's path under the directory.
	File       string
	Kind, Name string
	Action     Action
	// KeyKept is a Key that exists and is left as it is, whatever the
	// document says, because a Key's value is write-once.
	KeyKept bool
}

// Result is one run: the directory, whether it was the dry run, the
// files read, the three counts, and one outcome per document in the
// order the objects are written, kind order and path order within a
// kind. On a refusal it holds the outcomes before the refused document,
// and in an apply those objects are written.
type Result struct {
	Dir                         string
	DryRun                      bool
	Files                       int
	Created, Updated, Unchanged int
	Outcomes                    []Outcome
}

func (r *Result) add(o Outcome) {
	r.Outcomes = append(r.Outcomes, o)
	switch o.Action {
	case Created:
		r.Created++
	case Updated:
		r.Updated++
	default:
		r.Unchanged++
	}
}

// Lines are the start-up lines, one per outcome, in the shape of the
// file mode's: "bootstrap dir <dir>: <file>: <Kind> <name> <action>".
// Summary is the line printed after them.
func (r Result) Lines() []string {
	lines := make([]string, 0, len(r.Outcomes))
	for _, o := range r.Outcomes {
		action := string(o.Action)
		if r.DryRun && o.Action != Unchanged {
			action = "to be " + action
		}
		line := "bootstrap dir " + r.Dir + ": " + o.File + ": " + o.Kind + " " + o.Name + " " + action
		if o.KeyKept {
			line += "; a Key that exists is left as it is, because its value is write-once"
		}
		lines = append(lines, line)
	}
	return lines
}

// Summary is the run in one line: the files read and the three counts,
// and for the dry run that nothing was written.
func (r Result) Summary() string {
	if r.DryRun {
		return fmt.Sprintf("bootstrap dir %s: %d files read, %d to create, %d to update, %d unchanged; nothing written", r.Dir, r.Files, r.Created, r.Updated, r.Unchanged)
	}
	return fmt.Sprintf("bootstrap dir %s: %d files read, %d created, %d updated, %d unchanged", r.Dir, r.Files, r.Created, r.Updated, r.Unchanged)
}

// Apply reads the directory and writes what differs from the store. The
// whole directory is resolved before the first write, so a document that
// fails to decode, to validate, to resolve, or whose variable is unset
// writes nothing; a store failure during the writes ends the run with
// the objects before it written, which the next run finds unchanged.
func Apply(ctx context.Context, o Options) (Result, error) {
	return run(ctx, o, true)
}

// Check is Apply without the writes: it decodes, validates, and resolves
// the directory against the store and answers the counts an apply would
// produce, or the first refusal. It is the dry run luxd check reports.
func Check(ctx context.Context, o Options) (Result, error) {
	return run(ctx, o, false)
}

func run(ctx context.Context, o Options, write bool) (Result, error) {
	res := Result{Dir: o.Dir, DryRun: !write}
	if err := o.valid(); err != nil {
		return res, err
	}
	docs, err := readDir(o.Dir)
	if err != nil {
		return res, err
	}
	res.Files = len(docs)
	cat := newCatalog(o.Store.Objects())
	steps := make([]*step, 0, len(docs))
	for _, kind := range kindOrder {
		for _, d := range docs {
			if d.kind != kind {
				continue
			}
			s, err := o.plan(ctx, cat, d)
			if err != nil {
				return res, err
			}
			steps = append(steps, s)
		}
	}
	for _, s := range steps {
		if write && s.action != Unchanged {
			if err := o.write(ctx, s); err != nil {
				return res, err
			}
		}
		res.add(Outcome{File: s.doc.rel, Kind: s.doc.kind, Name: s.doc.name, Action: s.action, KeyKept: s.keyKept})
	}
	return res, nil
}

// step is one document resolved: the action, the object to write with
// the status the store is handed, the row it replaces, and what the
// write carries beside it.
type step struct {
	doc       *document
	action    Action
	keyKept   bool
	at        time.Time     // the resolve's clock and the event's time
	obj       v1.Object     // the resolved object
	existing  v1.Object     // the stored object an update replaces
	version   int64         // the version Put writes at; 0 creates
	hash      string        // a created Key's hash
	valuePath string        // the field a created Key's value arrived in
	sealed    *store.Sealed // a Provider's new credential
}

// plan reads the document's object by name and resolves the document
// against it, with the steps of the API's apply: the credential from the
// environment, Resolve with the existing object, the status the API
// fills, and, for a Provider or a Key, the credential or the hash.
func (o Options) plan(ctx context.Context, cat *catalog, d *document) (*step, error) {
	if d.name == "" {
		return nil, d.refuse(codeMissingField, "an object is read by its name before it is applied, so the name is required", "metadata.name")
	}
	s := &step{doc: d, at: o.now()}
	existing, version, err := o.existing(ctx, d)
	if err != nil {
		return nil, d.fail(err, "")
	}
	if d.kind == v1.KindKey && existing != nil {
		s.action, s.keyKept = Unchanged, true
		return s, nil
	}
	valuePath, rerr := o.fromEnvironment(ctx, cat, d, s.at)
	if rerr != nil {
		return nil, rerr
	}
	resolved, err := manifest.Resolve(ctx, d.obj, o.resolveOptions(cat, existing, s.at, false))
	if err != nil {
		return nil, fromVariable(d.fail(err, ""), valuePath)
	}
	obj := resolved.Object
	id, owner, createdAt := o.newID(prefixOf(d.kind)), o.Owner, s.at
	if existing != nil {
		id, owner, createdAt = existing.ID(), existing.Owner(), createdAtOf(existing)
	}
	setIdentity(obj, id, owner, createdAt)
	s.obj, s.existing, s.version = obj, existing, version
	changed := existing != nil && len(events.ChangedPaths(existing, obj)) > 0
	switch x := obj.(type) {
	case *v1.Provider:
		sealed, err := o.prepareProvider(ctx, s, x)
		if err != nil {
			return nil, err
		}
		changed = changed || sealed
	case *v1.Model:
		// The store replaces a discovered Model of the name in place only
		// for an object that says it is declared (spec 010), as the file
		// mode's does.
		x.Status.Source = v1.SourceDeclared
	case *v1.Key:
		if err := o.prepareKey(ctx, cat, s, x, valuePath); err != nil {
			return nil, err
		}
	}
	cat.add(obj)
	switch {
	case existing == nil:
		s.action = Created
	case changed:
		s.action = Updated
	default:
		s.action = Unchanged
	}
	return s, nil
}

// existing is the live object of the document's kind and name, nil and
// version 0 when there is none. A discovered Model does not count,
// because a declared one replaces it in place as a create (spec 010),
// which is how the API's apply reads it too.
func (o Options) existing(ctx context.Context, d *document) (v1.Object, int64, error) {
	obj, version, err := o.Store.Objects().ByName(ctx, d.kind, d.name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if m, ok := obj.(*v1.Model); ok && m.Status.Source == v1.SourceDiscovered {
		return nil, 0, nil
	}
	return obj, version, nil
}

// resolveOptions are the options Resolve runs under, the API's with no
// authorizer's limits and no name generator, because every object of the
// directory is read by the name it declares.
func (o Options) resolveOptions(cat *catalog, existing v1.Object, now time.Time, fileMode bool) manifest.Options {
	return manifest.Options{
		Actor:                 manifest.Actor{Subject: o.Owner},
		Lookup:                cat,
		Defaults:              o.Defaults,
		Existing:              existing,
		FileMode:              fileMode,
		AllowPrivateUpstreams: o.AllowPrivateUpstreams,
		TunnelEnabled:         o.TunnelEnabled,
		PublicURL:             o.PublicURL,
		Now:                   func() time.Time { return now },
	}
}

// fromEnvironment reads the variable a Provider's
// spec.credential.valueFrom.env or a Key's spec.valueFrom.env names, the
// rule the file mode states, and moves the value into the write-only
// field /v1 takes it in, spec.credential.value or spec.value, clearing
// valueFrom. The object then resolves, is sealed or hashed, and is
// stored exactly as a PUT carrying the value would store it, and stays
// writable through /v1, where valueFrom is the file mode's field and is
// refused. Before the move the document is resolved once as the file
// mode resolves it, which holds the variable's name and its exclusivity
// with a value to spec 003's rules.
//
// A Key with no variable, no value, and no hash is refused: the API
// would mint its value and return it once, and at start there is no
// caller to return it to. The answer is the path a created Key's value
// arrived at, which the refusal of a value another Key holds names.
func (o Options) fromEnvironment(ctx context.Context, cat *catalog, d *document, now time.Time) (string, *Error) {
	var (
		from *v1.ValueFrom
		path string
	)
	switch x := d.obj.(type) {
	case *v1.Provider:
		if x.Spec.Credential != nil {
			from, path = x.Spec.Credential.ValueFrom, "spec.credential.valueFrom.env"
		}
	case *v1.Key:
		from, path = x.Spec.ValueFrom, "spec.valueFrom.env"
		if from == nil {
			if _, ok := x.Spec.ValueSHA256(); ok {
				return "spec.valueSHA256", nil
			}
			if _, ok := x.Spec.Value(); ok {
				return "spec.value", nil
			}
			return "", d.refuse(codeMissingField, "a bootstrapped Key takes its value from the variable spec.valueFrom.env names, or its hash from spec.valueSHA256, because no caller is there to receive a value the server mints", path)
		}
	}
	if from == nil {
		return "", nil
	}
	if _, err := manifest.Resolve(ctx, d.obj, o.resolveOptions(cat, nil, now, true)); err != nil {
		return "", d.fail(err, "")
	}
	value := o.getenv(from.Env)
	if value == "" {
		return "", d.refuse(codeMissingField, "the variable "+from.Env+" is unset or empty", path)
	}
	switch x := d.obj.(type) {
	case *v1.Provider:
		x.Spec.Credential.ValueFrom = nil
		x.Spec.Credential.SetValue(value)
	case *v1.Key:
		x.Spec.ValueFrom = nil
		x.Spec.SetValue(value)
	}
	return path, nil
}

// fromVariable names the variable's field in a refusal of the value
// fromEnvironment moved out of it: spec.valueFrom.env for spec.value,
// spec.credential.valueFrom.env for spec.credential.value, so the
// refusal points at the field the operator's file carries. A path that
// names no variable leaves the refusal as it is.
func fromVariable(e *Error, path string) *Error {
	value, ok := strings.CutSuffix(path, "From.env")
	if !ok {
		return e
	}
	for i, p := range e.Paths {
		if p == value {
			e.Paths[i] = path
		}
	}
	return e
}

// prepareProvider is the credential half of the API's prepareProvider.
// A value the document carries is compared with the stored credential,
// opened under the keys, and sealed as the next version when it differs
// or none is stored, in the write's transaction; a value equal to the
// stored one, or no value at all, keeps the stored credential and its
// status, spec 003's update rule, so a restart reseals nothing. It
// reports whether a new credential is written.
func (o Options) prepareProvider(ctx context.Context, s *step, p *v1.Provider) (bool, *Error) {
	var stored *v1.CredentialStatus
	if old, ok := s.existing.(*v1.Provider); ok {
		stored = old.Status.Credential
	}
	value, carried := "", false
	if p.Spec.Credential != nil {
		value, carried = p.Spec.Credential.Value()
		p.Spec.Credential.ClearValue()
	}
	keep := func() {
		p.Status.Credential = stored
		if stored == nil {
			p.Status.Credential = &v1.CredentialStatus{}
		}
	}
	if !carried {
		keep()
		return false, nil
	}
	if o.Keys == nil {
		return false, s.doc.refuse(codeInternal, "LUX_SECRETS_KEK is not given, so the credential can be neither sealed nor compared", "spec.credential.value")
	}
	if stored != nil && stored.Set {
		same, err := o.sameCredential(ctx, s.doc, p.Status.ID, value)
		if err != nil {
			return false, err
		}
		if same {
			keep()
			return false, nil
		}
	}
	version := 1
	if stored != nil {
		version = stored.Version + 1
	}
	sealed, err := o.Keys.Seal(p.Status.ID, version, []byte(value))
	if err != nil {
		return false, s.doc.refuse(codeInternal, "sealing the credential: "+err.Error())
	}
	p.Status.Credential = &v1.CredentialStatus{Set: true, Version: version, UpdatedAt: s.at}
	s.sealed = &sealed
	return true, nil
}

// sameCredential reports whether the stored credential of the Provider
// is value. A row the keys do not open is refused rather than resealed,
// because a key encryption key that opens nothing is a configuration the
// start-up credential check refuses too, not a credential to replace.
func (o Options) sameCredential(ctx context.Context, d *document, providerID, value string) (bool, *Error) {
	row, err := o.Store.Credentials().Get(ctx, providerID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, d.fail(err, "")
	}
	plain, err := o.Keys.Open(providerID, row)
	if err != nil {
		return false, d.refuse(codeInternal, "the stored credential does not open under LUX_SECRETS_KEK: "+err.Error())
	}
	return subtle.ConstantTimeCompare(plain, []byte(value)) == 1, nil
}

// prepareKey is the create half of the API's prepareKey: status.budget
// from the catalog, and status.prefix and the hash of the value the
// environment or the document supplied, or of the hash the document
// carries, which the write registers in its transaction. The value
// leaves the object here. A fenced name, and a value another Key holds
// or another document of the run supplies, are refused before anything
// is written, so the dry run gives the answers the write would.
func (o Options) prepareKey(ctx context.Context, cat *catalog, s *step, k *v1.Key, valuePath string) *Error {
	if k.Spec.Budget != "" {
		b, err := cat.Budget(ctx, k.Spec.Budget)
		if err != nil {
			return s.doc.fail(err, "")
		}
		k.Status.Budget = &v1.BudgetRef{Name: b.Metadata.Name, ID: b.Status.ID}
	}
	value, _ := k.Spec.Value()
	hash, hashed := k.Spec.ValueSHA256()
	k.Spec.ClearValue()
	k.Spec.ClearValueSHA256()
	if hashed {
		k.Status.Prefix = serve.SuppliedKeyPrefix(hash)
	} else {
		hash = serve.HashKeyValue(value)
		k.Status.Prefix = serve.KeyPrefix(value, true)
	}
	_, err := o.Store.KeyFences().Get(ctx, k.Metadata.Name)
	switch {
	case err == nil:
		return s.doc.refuse(codeKeyFenced, "the Key name "+strconv.Quote(k.Metadata.Name)+" is fenced, closed to credential writes for good")
	case !errors.Is(err, store.ErrNotFound):
		return s.doc.fail(err, "")
	}
	if _, taken := cat.hashes[hash]; taken {
		return s.doc.refuse(codeInvalidField, hashTakenDetail, valuePath)
	}
	_, err = o.Store.Keys().ByHash(ctx, hash)
	switch {
	case err == nil:
		return s.doc.refuse(codeInvalidField, hashTakenDetail, valuePath)
	case !errors.Is(err, store.ErrNotFound):
		return s.doc.fail(err, "")
	}
	cat.hashes[hash] = k.Metadata.Name
	s.hash, s.valuePath = hash, valuePath
	return nil
}

// write is the API's one Transact for an apply: the object at the
// version it was read at, a Key's hash or a Provider's sealed credential,
// and the object's event, which commit together or not at all.
func (o Options) write(ctx context.Context, s *step) error {
	err := o.Store.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, s.obj, s.version); err != nil {
			return err
		}
		if s.hash != "" {
			if err := tx.Keys().Put(ctx, s.obj.ID(), s.hash); err != nil {
				return err
			}
		}
		if s.sealed != nil {
			if err := tx.Credentials().Put(ctx, s.obj.ID(), *s.sealed); err != nil {
				return err
			}
		}
		data := createdData(s.obj)
		if s.existing != nil {
			data = map[string]any{"paths": events.ChangedPaths(s.existing, s.obj)}
		}
		return events.Append(ctx, tx.Journal(), events.Event{
			ID: o.newID(v1.PrefixEvent), Type: eventType(s.doc.kind, s.existing == nil), At: s.at,
			Reason: events.ReasonBootstrap, Object: s.obj, Data: data,
		})
	})
	if err != nil {
		return s.doc.fail(err, s.valuePath)
	}
	return nil
}

// prefixOf is the id prefix of a kind.
func prefixOf(kind string) string {
	switch kind {
	case v1.KindProvider:
		return v1.PrefixProvider
	case v1.KindBudget:
		return v1.PrefixBudget
	case v1.KindModel:
		return v1.PrefixModel
	default:
		return v1.PrefixKey
	}
}

// setIdentity writes id, owner, and createdAt into the object's status,
// the three members the API fills before Put beside the kind's own.
func setIdentity(obj v1.Object, id, owner string, createdAt time.Time) {
	switch x := obj.(type) {
	case *v1.Provider:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt
	case *v1.Model:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt
	case *v1.Key:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt
	case *v1.Budget:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt
	}
}

// createdAtOf is the object's status.createdAt.
func createdAtOf(obj v1.Object) time.Time {
	switch x := obj.(type) {
	case *v1.Provider:
		return x.Status.CreatedAt
	case *v1.Model:
		return x.Status.CreatedAt
	case *v1.Key:
		return x.Status.CreatedAt
	case *v1.Budget:
		return x.Status.CreatedAt
	}
	return time.Time{}
}
