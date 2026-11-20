package storage

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// AWSUnitHistory is how far back one unit's kept snapshots reach: the
// oldest kept generation's start and how many generations are kept.
type AWSUnitHistory struct {
	Unit        AWSUnit   `json:"unit"`
	Oldest      time.Time `json:"oldest"`
	Generations int       `json:"generations"`
}

// AWSUnitHistoryStore answers how deep each unit's history is, across
// every organization in the read scope. Compare, before values and the
// alarm line check the actual oldest generation of each unit, never an
// assumed number of days. It is optional beside
// AWSInventoryStore and every backend with the inventory tables has it.
type AWSUnitHistoryStore interface {
	AWSUnitHistory(ctx context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitHistory, error)
}

var (
	_ AWSUnitHistoryStore = (*sqlTables)(nil)
	_ AWSUnitHistoryStore = (*memoryTables)(nil)
)

func sortUnitHistory(out []AWSUnitHistory) {
	sort.Slice(out, func(left, right int) bool { return lessAWSUnit(out[left].Unit, out[right].Unit) })
}

func (t *sqlTables) AWSUnitHistory(ctx context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitHistory, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	if len(scope.Read) == 1 {
		return t.awsUnitHistory(ctx, db, scope.Read[0], nil, query)
	}
	// Each organization's generations count only for the times the account
	// view lets it answer, so history reaches exactly as far as the views.
	byUnit := map[AWSUnit]*AWSUnitHistory{}
	err = t.readSnapshot(ctx, db, func(tx *sql.Tx) error {
		for position, org := range scope.Read {
			own, err := t.awsUnitHistory(ctx, tx, org, scope.Read[:position], query)
			if err != nil {
				return err
			}
			for _, item := range own {
				merged := byUnit[item.Unit]
				if merged == nil {
					copied := item
					byUnit[item.Unit] = &copied
					continue
				}
				merged.Generations += item.Generations
				if item.Oldest.Before(merged.Oldest) {
					merged.Oldest = item.Oldest
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]AWSUnitHistory, 0, len(byUnit))
	for _, item := range byUnit {
		out = append(out, *item)
	}
	sortUnitHistory(out)
	return out, nil
}

// awsUnitHistory counts one organization's generations per unit. With
// earlier organizations in read order, a generation counts only when it
// starts before each earlier organization's first generation of the unit
// and that first generation is still kept: once an earlier organization
// has pruned, its start is unknown and older organizations answer for
// nothing.
func (t *sqlTables) awsUnitHistory(ctx context.Context, q tablesQuerier, org string, earlier []string, query AWSUnitQuery) ([]AWSUnitHistory, error) {
	var where sqlWhere
	where.add("g.org_id = ?", org)
	where.equal("g.account", query.Account)
	where.equal("g.region", query.Region)
	where.equal("g.service", query.Service)
	if len(earlier) > 0 {
		where.add(`NOT EXISTS (SELECT 1 FROM aws_unit_generations h WHERE h.org_id IN (`+placeholders(len(earlier))+`)
 AND h.account = g.account AND h.region = g.region AND h.service = g.service
 AND (h.valid_from <= g.valid_from OR NOT EXISTS (SELECT 1 FROM aws_unit_generations f
 WHERE f.org_id = h.org_id AND f.account = h.account AND f.region = h.region AND f.service = h.service AND f.seq = 1)))`, stringArgs(earlier)...)
	}
	rows, err := q.QueryContext(ctx, t.q(`SELECT g.account, g.region, g.service, MIN(g.valid_from), COUNT(*) FROM aws_unit_generations g
 WHERE `+where.String()+` GROUP BY g.account, g.region, g.service ORDER BY g.account, g.region, g.service`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSUnitHistory, 0)
	for rows.Next() {
		var item AWSUnitHistory
		var oldest tableTimeValue
		if err := rows.Scan(&item.Unit.Account, &item.Unit.Region, &item.Unit.Service, &oldest, &item.Generations); err != nil {
			return nil, err
		}
		item.Oldest = oldest.Time
		out = append(out, item)
	}
	return out, rows.Err()
}

func (m *memoryTables) AWSUnitHistory(_ context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitHistory, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	match := func(unit AWSUnit) bool { return matchAWSUnit(unit, query.Account, query.Region, query.Service) }
	generations := make([]map[AWSUnit][]time.Time, 0, len(scope.Read))
	bounds := make([]map[AWSUnit]awsGenerationBounds, 0, len(scope.Read))
	for _, org := range scope.Read {
		starts := map[AWSUnit][]time.Time{}
		for key, entry := range m.units {
			if key.org != org || entry == nil || !match(key.unit) {
				continue
			}
			for _, version := range entry.generations {
				starts[key.unit] = append(starts[key.unit], version.from)
			}
		}
		generations = append(generations, starts)
		bounds = append(bounds, m.generationBoundsLocked(org, match))
	}
	return mergeAWSUnitHistory(generations, bounds), nil
}

func (p *postgresProvider) AWSUnitHistory(ctx context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitHistory, error) {
	return p.tablesRead().AWSUnitHistory(ctx, scope, query)
}
