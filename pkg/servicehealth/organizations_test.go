package servicehealth_test

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/servicehealth"
)

func TestOrganizationsDefaultsAndNormalizesRegisteredOrganizations(t *testing.T) {
	servicehealth.SetOrganizationLister(nil)
	t.Cleanup(func() { servicehealth.SetOrganizationLister(nil) })
	organizations, err := servicehealth.Organizations(context.Background())
	if err != nil || len(organizations) != 1 || organizations[0] != "default" {
		t.Fatalf("default organizations = %#v, err %v", organizations, err)
	}
	servicehealth.SetOrganizationLister(func(context.Context) ([]string, error) {
		return []string{"org-a", "", "org-a", "org-b"}, nil
	})
	organizations, err = servicehealth.Organizations(context.Background())
	if err != nil || len(organizations) != 3 || organizations[0] != "default" || organizations[1] != "org-a" || organizations[2] != "org-b" {
		t.Fatalf("registered organizations = %#v, err %v", organizations, err)
	}
}
