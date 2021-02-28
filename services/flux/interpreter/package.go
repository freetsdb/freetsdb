package interpreter

import "github.com/freetsdb/freetsdb/services/flux/values"

// Package is the internal representation of a Flux source file.
// A package is modeled as an object where the keys are the
// exported identifiers.
type Package interface {
	values.Object
	Name() string
	SideEffects() []values.Value
}
