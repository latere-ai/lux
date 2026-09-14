// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// APIVersion is the one group and version this package serves.
const APIVersion = "lux.latere.ai/v1beta1"

// LabelPrefix is reserved for the gateway: a label or annotation key under
// it is refused, and the gateway writes none under it in v1beta1, so an
// object read back re-applies unchanged.
const LabelPrefix = "lux.latere.ai/"

// The four kinds, as the kind field spells them.
const (
	KindProvider = "Provider"
	KindModel    = "Model"
	KindKey      = "Key"
	KindBudget   = "Budget"
)

// KnownKind reports whether k is one of the four kinds.
func KnownKind(k string) bool {
	switch k {
	case KindProvider, KindModel, KindKey, KindBudget:
		return true
	default:
		return false
	}
}

// The id prefixes. One per kind, and the three of the other objects NewID
// mints: a request, an event, and a tunnel session.
const (
	PrefixProvider = "prv_"
	PrefixModel    = "mdl_"
	PrefixKey      = "key_"
	PrefixBudget   = "bud_"
	PrefixRequest  = "req_"
	PrefixEvent    = "evt_"
	PrefixTunnel   = "tun_"
)

// KindPrefixes are the id prefixes no name of any kind may begin with, so
// that a path segment carrying one is an id and any other is a name.
var KindPrefixes = []string{PrefixProvider, PrefixModel, PrefixKey, PrefixBudget}

// Object is what every kind implements. Decode returns it and the store
// keys by it. The kind is the Go type's and the id and owner are the
// status's, so a caller building a literal fills neither.
type Object interface {
	Kind() string
	ID() string
	Owner() string
	Name() string
}

// ObjectMeta is the metadata of every kind.
type ObjectMeta struct {
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// envelope is the apiVersion and kind every encoding carries first. The
// kinds do not store them: a Go value's kind is its type and its version
// is this package's, so each kind's MarshalJSON writes both and a plain
// json.Unmarshal reads past them.
type envelope struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// writeOnly is the value of a field the encoders skip: the string the
// manifest carried and whether it carried one at all, so an empty string
// that was written is told from a field that was absent.
type writeOnly struct {
	value string
	set   bool
}

func (w *writeOnly) get() (string, bool) { return w.value, w.set }
func (w *writeOnly) put(v string)        { w.value, w.set = v, true }
func (w *writeOnly) clear()              { w.value, w.set = "", false }
