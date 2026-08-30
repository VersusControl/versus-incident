package tools

import "testing"

func TestCatalogIsCompleteOrderedAndUnique(t *testing.T) {
	wantGroups := []Group{
		GroupVersus, GroupVersus, GroupVersus, GroupVersus, GroupVersus, GroupVersus,
		GroupVersus, GroupVersus, GroupVersus, GroupVersus, GroupVersus,
		GroupCommon, GroupCommon, GroupCommon, GroupCommon, GroupCommon, GroupCommon,
		GroupK8s, GroupK8s, GroupK8s, GroupK8s, GroupK8s,
	}
	got := Catalog()
	if len(got) != len(wantGroups) {
		t.Fatalf("Catalog() has %d tools, want %d", len(got), len(wantGroups))
	}
	seen := make(map[string]struct{}, len(got))
	for i, tool := range got {
		if tool.Group != wantGroups[i] {
			t.Errorf("Catalog()[%d].Group = %q, want %q", i, tool.Group, wantGroups[i])
		}
		if tool.Name == "" || tool.DisplayName == "" || tool.Description == "" {
			t.Errorf("Catalog()[%d] has incomplete metadata: %#v", i, tool)
		}
		if _, duplicate := seen[tool.Name]; duplicate {
			t.Errorf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
}

func TestCatalogDeclaresCompoundRunbookCapabilities(t *testing.T) {
	for _, tool := range Catalog() {
		if tool.Name != "find_runbook" {
			continue
		}
		if tool.Requirement.Kind != RequirementCapability || len(tool.Requirement.Capabilities) != 2 {
			t.Fatalf("find_runbook requirement = %#v, want two capabilities", tool.Requirement)
		}
		return
	}
	t.Fatal("find_runbook missing from catalog")
}

func TestCatalogCopyIsDetached(t *testing.T) {
	got := Catalog()
	got[0].Name = "changed"
	got[14].Requirement.Capabilities[0] = "changed"
	if Catalog()[0].Name == "changed" {
		t.Fatal("Catalog returned mutable backing storage")
	}
	if Catalog()[14].Requirement.Capabilities[0] == "changed" {
		t.Fatal("Catalog returned mutable requirement capabilities")
	}
}
