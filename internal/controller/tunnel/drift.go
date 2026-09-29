package tunnel

import (
	"reflect"

	"k8s.io/apimachinery/pkg/api/equality"
)

// drifted reports whether a live spec of an owned object no longer carries the desired spec.
//
// The API server fills in defaults (a Deployment's strategy, a container's
// terminationMessagePath, …), so the live spec never equals the desired one. The comparison is
// therefore derivative: a field the desired spec leaves empty (zero string, nil pointer, empty
// slice or map) matches anything, and every field it sets must match. Unlike
// equality.Semantic.DeepDerivative, a non-empty desired list must have exactly as many live
// entries: a container, env var, argument or egress rule added by hand counts as drift. Extra
// map keys (labels, annotations such as kubectl's restartedAt) are tolerated.
//
// So a reconcile without spec changes does not rewrite the object, while a manual edit of a
// field the controller sets is reverted.
func drifted(desired, live any) bool {
	return !derives(reflect.ValueOf(desired), reflect.ValueOf(live))
}

func derives(d, l reflect.Value) bool {
	if !d.IsValid() || !l.IsValid() {
		return d.IsValid() == l.IsValid()
	}
	if d.Type() != l.Type() {
		return false
	}
	if _, ok := equality.Semantic.Equalities[d.Type()]; ok { // Quantity, Time, …
		return equality.Semantic.DeepDerivative(d.Interface(), l.Interface())
	}
	switch d.Kind() {
	case reflect.Pointer, reflect.Interface:
		if d.IsNil() {
			return true
		}
		if l.IsNil() {
			return false
		}
		return derives(d.Elem(), l.Elem())
	case reflect.Slice:
		if d.Len() == 0 {
			return true
		}
		if d.Len() != l.Len() {
			return false
		}
		for i := 0; i < d.Len(); i++ {
			if !derives(d.Index(i), l.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		if !allExported(d.Type()) {
			return equality.Semantic.DeepDerivative(d.Interface(), l.Interface())
		}
		for i := 0; i < d.NumField(); i++ {
			if !derives(d.Field(i), l.Field(i)) {
				return false
			}
		}
		return true
	default: // maps and scalars
		return equality.Semantic.DeepDerivative(d.Interface(), l.Interface())
	}
}

func allExported(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if !t.Field(i).IsExported() {
			return false
		}
	}
	return true
}
