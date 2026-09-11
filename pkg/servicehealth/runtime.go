package servicehealth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// ScheduleJobName is the stable ownership key for Service Health collection.
const ScheduleJobName = "service-health"

// RuntimeOptions supplies generic local readers for one Service Health runtime.
type RuntimeOptions struct {
	Manager   *Manager
	Store     storage.Provider
	Services  func() []ServiceMetadata
	Patterns  func() []PatternMetadata
	SourceIDs []string
	Now       func() time.Time
}

// Runtime carries the manager mounted by routes and its shared scheduler job.
type Runtime struct {
	Manager *Manager
	Job     scheduler.Job
}

// NewRuntime constructs one manager and one organization-aware scheduler job.
// The caller installs any organization lister and ownership predicate before
// registering Job with the shared scheduler.
func NewRuntime(options RuntimeOptions) *Runtime {
	manager := options.Manager
	if manager == nil {
		manager = NewManager(options.Store)
	}
	collector := NewCollector(CollectorOptions{
		Manager: manager, Store: options.Store, Services: options.Services,
		Patterns: options.Patterns, SourceIDs: options.SourceIDs, Now: options.Now,
	})
	runtime := &Runtime{Manager: manager}
	runtime.Job = scheduler.Job{
		Name:     ScheduleJobName,
		Interval: time.Duration(DefaultIntervalSeconds) * time.Second,
		ResolveInterval: func() time.Duration {
			interval := time.Duration(DefaultIntervalSeconds) * time.Second
			organizations, err := Organizations(context.Background())
			if err != nil {
				return interval
			}
			for _, orgID := range organizations {
				settings, loadErr := manager.LoadSettings(orgID)
				if loadErr == nil && time.Duration(settings.IntervalSeconds)*time.Second < interval {
					interval = time.Duration(settings.IntervalSeconds) * time.Second
				}
			}
			return interval
		},
		Run: func(ctx context.Context) error {
			organizations, err := Organizations(ctx)
			if err != nil {
				return err
			}
			var collectionErrors []error
			for _, orgID := range organizations {
				if _, collectErr := collector.Collect(ctx, orgID); collectErr != nil {
					collectionErrors = append(collectionErrors, fmt.Errorf("collect organization %q: %w", orgID, collectErr))
				}
			}
			return errors.Join(collectionErrors...)
		},
	}
	return runtime
}
