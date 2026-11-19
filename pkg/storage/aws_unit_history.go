package storage

import (
	"context"
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

// AWSUnitHistoryStore answers how deep each unit's history is. Compare,
// before values and the alarm line check the actual oldest generation of
// each unit, never an assumed number of days. It is optional beside
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
	var where sqlWhere
	condition, args := visible(scope.Read, "g", "aws_sync_units", "account", "region", "service")
	where.add(condition, args...)
	where.equal("g.account", query.Account)
	where.equal("g.region", query.Region)
	where.equal("g.service", query.Service)
	rows, err := db.QueryContext(ctx, t.q(`SELECT g.account, g.region, g.service, MIN(g.valid_from), COUNT(*) FROM aws_unit_generations g
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
	out := make([]AWSUnitHistory, 0)
	for _, key := range m.visibleUnits(scope) {
		if !matchAWSUnit(key.unit, query.Account, query.Region, query.Service) {
			continue
		}
		entry := m.units[key]
		if entry == nil || len(entry.generations) == 0 {
			continue
		}
		item := AWSUnitHistory{Unit: key.unit, Generations: len(entry.generations)}
		for _, version := range entry.generations {
			if item.Oldest.IsZero() || version.from.Before(item.Oldest) {
				item.Oldest = version.from
			}
		}
		out = append(out, item)
	}
	sortUnitHistory(out)
	return out, nil
}

func (p *postgresProvider) AWSUnitHistory(ctx context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitHistory, error) {
	return p.tablesRead().AWSUnitHistory(ctx, scope, query)
}
