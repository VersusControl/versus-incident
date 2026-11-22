package breaklogs

import (
	"slices"
	"strings"
	"testing"
)

// FuzzScopeAllows checks the scope rule on arbitrary names: it never panics,
// an empty namespace is never allowed, an excluded namespace is never
// allowed even when it is also included, an empty include list allows every
// other name, and a non-empty one allows only the names it lists. Names
// match exactly, so a case or whitespace variant is a different name.
func FuzzScopeAllows(f *testing.F) {
	f.Add("shop", "", "kube-system", "", "shop")
	f.Add("", "", "", "", "")
	f.Add("shop", "kube-system", "kube-system", "", "kube-system")
	f.Add("", "", "shop", "", "Shop")
	f.Add("shop", "", "", "", " shop")
	f.Add("\x00", "", "", "\x00", "\x00")
	f.Fuzz(func(t *testing.T, include1, include2, exclude1, exclude2, namespace string) {
		nonEmpty := func(values ...string) []string {
			return slices.DeleteFunc(values, func(value string) bool { return value == "" })
		}
		scope := Scope{Include: nonEmpty(include1, include2), Exclude: nonEmpty(exclude1, exclude2)}
		got := scope.Allows(namespace)
		excluded := slices.Contains(scope.Exclude, namespace)
		var want bool
		switch {
		case namespace == "", excluded:
			want = false
		case len(scope.Include) == 0:
			want = true
		default:
			want = slices.Contains(scope.Include, namespace)
		}
		if got != want {
			t.Fatalf("Scope%+v.Allows(%q) = %v, want %v", scope, namespace, got, want)
		}
		if scope.Allows("") {
			t.Fatalf("Scope%+v allowed the empty namespace", scope)
		}
		// Exclude always wins, whatever the include list says.
		for _, name := range scope.Exclude {
			widened := Scope{Include: append(slices.Clone(scope.Include), name), Exclude: scope.Exclude}
			if widened.Allows(name) {
				t.Fatalf("Scope%+v allowed excluded %q", widened, name)
			}
		}
		// A variant of an included name is not the name.
		if got && len(scope.Include) > 0 {
			for _, variant := range []string{strings.ToUpper(namespace), " " + namespace, namespace + " ", namespace + "\x00"} {
				if variant != namespace && !slices.Contains(scope.Include, variant) && scope.Allows(variant) {
					t.Fatalf("Scope%+v allowed %q, a variant of %q", scope, variant, namespace)
				}
			}
		}
		// The call reads the scope and never changes it.
		before := Scope{Include: slices.Clone(scope.Include), Exclude: slices.Clone(scope.Exclude)}
		scope.Allows(namespace)
		if !slices.Equal(before.Include, scope.Include) || !slices.Equal(before.Exclude, scope.Exclude) {
			t.Fatalf("Allows changed the scope: %+v -> %+v", before, scope)
		}
	})
}
