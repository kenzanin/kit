package extensions

import (
	"reflect"
	"strings"
	"testing"
)

// TestNativeBindingParity fails the build when API grows a new On* method
// without a native binding, or when nativeEvents names a vanished method.
// Register* ops are hand-wrapped on nativeBridge; the compiler catches
// those. OnCustomEvent is the deliberate escape hatch for unknown names.
func TestNativeBindingParity(t *testing.T) {
	apiType := reflect.PointerTo(reflect.TypeOf(API{}))
	seen := map[string]bool{}

	for event, meth := range nativeEvents {
		if !hasMethod(apiType, meth) {
			t.Errorf("nativeEvents[%q]: no API method %s", event, meth)
			continue
		}
		if seen[meth] {
			t.Errorf("method %s bound to more than one event", meth)
		}
		seen[meth] = true
	}

	for i := 0; i < apiType.NumMethod(); i++ {
		name := apiType.Method(i).Name
		if !strings.HasPrefix(name, "On") || name == "OnCustomEvent" {
			continue
		}
		if !seen[name] {
			t.Errorf("API.%s has no native binding: add it to nativeEvents in native.go", name)
		}
	}
}

func hasMethod(t reflect.Type, name string) bool {
	m, ok := t.MethodByName(name)
	return ok && m.Func.IsValid()
}
