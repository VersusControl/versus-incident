package issues

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// Source gives the evaluator the accounts to evaluate and a view of each.
// It reads stores only; it never calls AWS.
type Source interface {
	Accounts(ctx context.Context) ([]string, error)
	View(ctx context.Context, account string) (View, error)
}

// SnapshotReader is the part of the snapshot store a SnapshotSource reads.
type SnapshotReader interface {
	Accounts() ([]string, error)
	Resources(ctx context.Context, query snapshot.Query) (snapshot.ResourcesResult, error)
	Health(ctx context.Context, query snapshot.Query) (snapshot.HealthResult, error)
	Sync(ctx context.Context, query snapshot.Query) ([]snapshot.UnitStatus, error)
}

// SnapshotSource reads views from the collectors' snapshot store.
type SnapshotSource struct{ store SnapshotReader }

// NewSnapshotSource returns a source over the snapshot store.
func NewSnapshotSource(store SnapshotReader) *SnapshotSource { return &SnapshotSource{store: store} }

// Accounts lists the accounts the store holds data for.
func (source *SnapshotSource) Accounts(context.Context) ([]string, error) {
	return source.store.Accounts()
}

// View reads one account's current resources, health, gaps and the latest
// read of every unit service.
func (source *SnapshotSource) View(ctx context.Context, account string) (View, error) {
	query := snapshot.Query{Account: account}
	resources, err := source.store.Resources(ctx, query)
	if err != nil {
		return View{}, err
	}
	health, err := source.store.Health(ctx, query)
	if err != nil {
		return View{}, err
	}
	units, err := source.store.Sync(ctx, query)
	if err != nil {
		return View{}, err
	}
	return NewView(ViewData{
		Account: account, AsOf: AsOfUnits(units), Resources: resources.Resources, Health: health.Health,
		Gaps: append(append([]model.Gap{}, resources.Gaps...), health.Gaps...), Reads: ReadsOf(units),
	}), nil
}

// AsOfUnits is when the view's data was last read: the latest time a unit
// committed or confirmed its snapshot. It is zero only when no unit has.
func AsOfUnits(units []snapshot.UnitStatus) time.Time {
	var asOf time.Time
	for _, status := range units {
		for _, at := range []time.Time{status.CommittedAt, status.VerifiedAt} {
			if at.After(asOf) {
				asOf = at
			}
		}
	}
	return asOf.UTC()
}

// ReadsOf folds unit statuses into the latest read of each service in each
// Region: a unit
// never attempted is not read, a unit with gaps (a failure, or a detail
// call inside a committed read) is read as its gaps, and any other
// committed unit is read.
func ReadsOf(units []snapshot.UnitStatus) map[ReadKey]Read {
	reads := map[ReadKey]Read{}
	for _, status := range units {
		var read Read
		switch status.Sync.State {
		case model.SyncOK, model.SyncFailed:
			read = readOfGaps(status.Sync.Gaps)
			if read == "" {
				read = ReadOK
				if status.Sync.State == model.SyncFailed {
					read = ReadFor(model.OutcomeForReason(model.GapReasonForCode(status.Sync.ErrorCode)))
				}
			}
		default:
			read = ReadNotRead
		}
		key := ReadKey{Service: status.Unit.Service, Region: status.Unit.Region}
		if kept, ok := reads[key]; ok {
			read = Combine(kept, read)
		}
		reads[key] = read
	}
	return reads
}

// readOfGaps folds one unit's gaps; a gap decides the unit's read, so a
// detector that is not enabled reads as off in AWS. No gaps is "".
func readOfGaps(gaps []model.Gap) Read {
	var read Read
	for index, gap := range gaps {
		if index == 0 {
			read = ReadFor(gap.Outcome())
			continue
		}
		read = Combine(read, ReadFor(gap.Outcome()))
	}
	return read
}
