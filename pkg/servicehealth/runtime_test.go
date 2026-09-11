package servicehealth_test

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestNewRuntimeProvidesSharedManagerAndOrganizationAwareJob(t *testing.T) {
	servicehealth.SetOrganizationLister(func(context.Context) ([]string, error) { return []string{"org-a"}, nil })
	scheduler.SetOwnership(func(name string) bool { return name == servicehealth.ScheduleJobName })
	t.Cleanup(func() {
		servicehealth.SetOrganizationLister(nil)
		scheduler.SetOwnership(nil)
	})
	runtime := servicehealth.NewRuntime(servicehealth.RuntimeOptions{Store: storage.NewMemory(), Now: func() time.Time {
		return time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	}})
	if runtime.Manager == nil || runtime.Job.Name != servicehealth.ScheduleJobName || !scheduler.Owns(runtime.Job.Name) {
		t.Fatalf("runtime=%#v", runtime)
	}
	if err := runtime.Job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := runtime.Manager.LoadSnapshot("org-a"); err != nil || !ok {
		t.Fatalf("organization snapshot exists=%v err=%v", ok, err)
	}
}
