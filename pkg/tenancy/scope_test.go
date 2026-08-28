package tenancy

import (
	"reflect"
	"testing"
)

func TestDefaultOrgScope(t *testing.T) {
	got := DefaultOrgScope()
	if got.Write != DefaultOrgID {
		t.Fatalf("Write = %q, want %q", got.Write, DefaultOrgID)
	}
	if !reflect.DeepEqual(got.Read, []string{DefaultOrgID}) {
		t.Fatalf("Read = %v, want [%q]", got.Read, DefaultOrgID)
	}
}

func TestNewOrgScopeWriteFirstAndDeduplicated(t *testing.T) {
	got := NewOrgScope("licensed", DefaultOrgID, "licensed", "legacy", DefaultOrgID)
	want := OrgScope{Write: "licensed", Read: []string{"licensed", DefaultOrgID, "legacy"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NewOrgScope = %#v, want %#v", got, want)
	}
}

func TestOrgScopeNormalizesBlankAndOwnsReadSlice(t *testing.T) {
	read := []string{"", "legacy"}
	scope := NewOrgScope("", read...)
	read[1] = "mutated"
	if !reflect.DeepEqual(scope.Read, []string{DefaultOrgID, "legacy"}) {
		t.Fatalf("Read = %v, want [%q legacy]", scope.Read, DefaultOrgID)
	}

	orgs := scope.OrgIDs()
	orgs[0] = "mutated"
	if scope.Read[0] != DefaultOrgID {
		t.Fatalf("OrgIDs returned aliased storage: scope.Read = %v", scope.Read)
	}
}
