package catalog

import "testing"

func TestCatalogHasTwentyFourUniqueServices(t *testing.T) {
	services := Services()
	if len(services) != 24 {
		t.Fatalf("expected 24 services, got %d", len(services))
	}
	seen := make(map[string]bool, len(services))
	for _, service := range services {
		if service.ID == "" || service.DisplayName == "" || service.Operation == "" {
			t.Errorf("service %q has an empty field", service.ID)
		}
		if seen[service.ID] {
			t.Errorf("duplicate service ID %q", service.ID)
		}
		seen[service.ID] = true
		if len(service.FixActions) == 0 {
			t.Errorf("service %q has no fix actions", service.ID)
		}
		for _, action := range service.FixActions {
			if action == "" {
				t.Errorf("service %q has an empty fix action", service.ID)
			}
		}
		switch service.Scope {
		case ScopeRegion, ScopeGlobalHome:
		default:
			t.Errorf("service %q has unknown scope %q", service.ID, service.Scope)
		}
	}
}

func TestCatalogByIDReturnsACopy(t *testing.T) {
	service, ok := ByID("s3")
	if !ok || service.DisplayName != "S3" {
		t.Fatalf("expected S3, got %#v, %v", service, ok)
	}
	service.FixActions[0] = "changed"
	again, _ := ByID("s3")
	if again.FixActions[0] != "s3:ListAllMyBuckets" {
		t.Fatal("ByID exposed the catalog's slice")
	}

	first := Services()
	first[0].FixActions[0] = "changed"
	second := Services()
	if second[0].FixActions[0] != "apigateway:GET" {
		t.Fatal("Services exposed the catalog's slice")
	}
}

func TestServiceRegion(t *testing.T) {
	regional, _ := ByID("ec2")
	if got := regional.Region("eu-west-1"); got != "eu-west-1" {
		t.Fatalf("regional service used %q", got)
	}
	if got := regional.Region(""); got != GlobalHomeRegion {
		t.Fatalf("regional service without a home used %q", got)
	}
	global, _ := ByID("iam")
	if got := global.Region("ap-southeast-2"); got != GlobalHomeRegion {
		t.Fatalf("global service used %q", got)
	}
}

func TestCatalogUnknownID(t *testing.T) {
	if _, ok := ByID("does-not-exist"); ok {
		t.Fatal("unknown ID resolved")
	}
}
