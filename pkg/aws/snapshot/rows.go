package snapshot

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The inventory tables key rows by the unit that holds them. A resource,
// health signal or flag keeps the fields the tables do not query in its
// detail document, so a read returns exactly what was committed.

type resourceDetail struct {
	Service   string `json:"service"`
	Region    string `json:"region"`
	AppSource string `json:"app_source,omitempty"`
	AppTag    string `json:"app_tag,omitempty"`
}

type regionDetail struct {
	Region string `json:"region"`
}

func storageUnit(unit model.Unit) storage.AWSUnit {
	return storage.AWSUnit{Account: unit.Account, Region: unit.Region, Service: unit.Service}
}

func modelUnit(unit storage.AWSUnit) model.Unit {
	return model.Unit{Account: unit.Account, Region: unit.Region, Service: unit.Service}
}

func marshalDetail(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func resourceRow(resource model.Resource) storage.AWSResource {
	relations := make([]storage.AWSRelation, 0, len(resource.Relations))
	for _, relation := range resource.Relations {
		relations = append(relations, storage.AWSRelation{Type: string(relation.Type), Target: relation.Target})
	}
	return storage.AWSResource{
		ARN: resource.ARN, Type: resource.Type, App: resource.App, Name: resource.Name, State: resource.State,
		Tags: resource.Tags, Relations: relations, Attributes: resource.Attributes,
		Detail: marshalDetail(resourceDetail{Service: resource.Service, Region: resource.Region, AppSource: resource.AppSource, AppTag: resource.AppTag}),
	}
}

func resourceOf(row storage.AWSResource) model.Resource {
	var detail resourceDetail
	_ = json.Unmarshal(row.Detail, &detail)
	resource := model.Resource{
		ARN: row.ARN, Service: detail.Service, Type: row.Type, Account: row.Account, Region: detail.Region,
		App: row.App, AppSource: detail.AppSource, AppTag: detail.AppTag, Name: row.Name, State: row.State, Tags: row.Tags, Attributes: row.Attributes,
	}
	for _, relation := range row.Relations {
		resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationType(relation.Type), Target: relation.Target})
	}
	return resource
}

func healthRow(item model.Health) storage.AWSHealth {
	return storage.AWSHealth{
		ARN: item.Resource, Kind: item.Source, Ref: item.Ref, State: string(item.Status), Summary: item.Summary,
		Since: item.Since, Children: item.Children, Detail: marshalDetail(regionDetail{Region: item.Region}),
	}
}

func healthOf(row storage.AWSHealth) model.Health {
	var detail regionDetail
	_ = json.Unmarshal(row.Detail, &detail)
	return model.Health{
		Resource: row.ARN, Account: row.Account, Region: detail.Region, Source: row.Kind, Status: model.HealthStatus(row.State),
		Summary: row.Summary, Since: row.Since, Ref: row.Ref, Children: row.Children,
	}
}

func flagRow(flag model.SecurityFlag) storage.AWSSecurityFlag {
	return storage.AWSSecurityFlag{ARN: flag.Resource, Flag: string(flag.Kind), Summary: flag.Summary, Detail: marshalDetail(regionDetail{Region: flag.Region})}
}

func flagOf(row storage.AWSSecurityFlag) model.SecurityFlag {
	var detail regionDetail
	_ = json.Unmarshal(row.Detail, &detail)
	return model.SecurityFlag{Resource: row.ARN, Account: row.Account, Region: detail.Region, Kind: model.SecurityFlagKind(row.Flag), Summary: row.Summary}
}

// changeRow stores the whole change as its detail; the columns carry what
// reads filter on.
func changeRow(region string, change model.Change) storage.AWSChange {
	row := storage.AWSChange{
		EventID: change.ID, Account: change.Account, Region: region, Service: change.Service, EventTime: change.Time,
		EventName: change.EventName, ActorClass: string(change.Actor.Class), Actor: change.Actor.PrincipalARN,
		Summary: change.Summary, Fields: change.Fields, Detail: marshalDetail(change),
	}
	if len(change.Resources) > 0 {
		row.ResourceARN = change.Resources[0]
	}
	return row
}

func changeOf(row storage.AWSChange) (model.Change, bool) {
	var change model.Change
	if json.Unmarshal(row.Detail, &change) != nil || change.ID != row.EventID {
		return model.Change{}, false
	}
	return change, true
}

func encodeGaps(gaps []model.Gap) json.RawMessage {
	if len(gaps) == 0 {
		return nil
	}
	return marshalDetail(gaps)
}

// decodeGaps reads a stored coverage document. An empty document means no
// gaps; a document that cannot be decoded is an error, never an empty list,
// because a gap is never rendered as zero.
func decodeGaps(data json.RawMessage) ([]model.Gap, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var gaps []model.Gap
	if err := json.Unmarshal(data, &gaps); err != nil {
		return nil, err
	}
	if len(gaps) == 0 {
		return nil, nil
	}
	return gaps, nil
}

// unreadableGaps represents a stored coverage document the store cannot
// decode the way a denied read is represented: the unit's own data is named
// as denied rather than read as an absence of gaps.
func unreadableGaps(unit model.Unit, lastGoodAt time.Time) []model.Gap {
	return []model.Gap{{Account: unit.Account, Region: unit.Region, Service: unit.Service, Reason: model.GapDenied, LastGoodAt: lastGoodAt}}
}

// generationGaps is the committed detail of one unit's generation. An
// unreadable document becomes a denied gap; it is never silently dropped.
func generationGaps(unit storage.AWSUnitStatus) []model.Gap {
	if unit.Generation == nil {
		return nil
	}
	gaps, err := decodeGaps(unit.Generation.Gaps)
	if err != nil {
		return unreadableGaps(modelUnit(unit.Unit), unit.Generation.CommittedAt)
	}
	return gaps
}

// syncOf reads a unit's sync state and reports whether its coverage
// document was readable. An unreadable document marks the state failed; the
// caller names the denied gap with the unit it belongs to.
func syncOf(state storage.AWSSyncState) (model.SyncState, bool) {
	gaps, err := decodeGaps(state.Gaps)
	if err != nil {
		return model.SyncState{State: model.SyncFailed, LastGoodAt: state.LastGoodAt, AttemptedAt: state.AttemptedAt, ErrorCode: state.ErrorCode}, false
	}
	return model.SyncState{State: model.SyncStatus(state.State), LastGoodAt: state.LastGoodAt, AttemptedAt: state.AttemptedAt, ErrorCode: state.ErrorCode, Gaps: gaps}, true
}

// storeError maps the tables' errors onto the package's own.
func storeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrAWSInventoryFenced):
		return ErrPurged
	case errors.Is(err, storage.ErrInvalidAWSInventory):
		return ErrInvalidCandidate
	}
	return err
}
