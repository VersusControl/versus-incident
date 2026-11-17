package cost

import (
	"math/big"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// cell is one Cost Explorer group: two keys, the net amortized cost and the
// usage quantity, in micro-units.
type cell struct {
	A string `json:"a,omitempty"`
	B string `json:"b,omitempty"`
	M int64  `json:"m"`
	Q int64  `json:"q,omitempty"`
	U string `json:"u,omitempty"`
}

// dayTables are the daily tables of one payer and day, as Cost Explorer
// returned them. A refresh turns them into cost lines for the FinOps
// tables; a later partial update (a manual refresh, a drill-down, resource
// detail) rebuilds them from the stored lines first (tablesOf).
type dayTables struct {
	// Account: A=linked account, B=service (usage records only).
	Account []cell
	// Usage: A=service, B=usage type (usage records only).
	Usage []cell
	// Team: A=tag group key ("team$checkout"), B=service, or B=linked
	// account when TeamBy is "account" (usage records).
	Team   []cell
	TeamBy string
	// Record: A=record type, B=linked account (non-usage records).
	Record []cell
	// Drill holds usage-type and team detail for one account and service
	// of an Organization ("account|service" → A=usage type, B=tag group
	// key). It makes that pair's team and region exact and never changes a
	// total.
	Drill map[string][]cell
	// Resource holds resource-level detail: A=usage type, B=resource ID.
	Resource []cell
	// regions, when set, is each service's region mix, used instead of the
	// usage table: tables rebuilt from an Organization's stored lines have
	// no usage table.
	regions map[string][]weight
}

// total is the day's cost: usage by account plus every non-usage record.
func (t *dayTables) total() int64 {
	var s int64
	for _, c := range t.Account {
		s += c.M
	}
	for _, c := range t.Record {
		if !isUsageRecord(c.A) {
			s += c.M
		}
	}
	return s
}

// teamByAccount marks a team table grouped by linked account.
const teamByAccount = "account"

// synthOptions carry what line building needs besides the tables.
type synthOptions struct {
	payer        string
	organization bool
	currency     string
	keepTeams    bool
	// detail keeps an Organization's drilled pairs at usage-type level
	// (each drill cell its own line under the pair's stable service key),
	// the shape the cost tables store; reads fold them back (foldDrills).
	detail bool
}

// buildLines turns one day's tables into cost lines.
//
// Cost Explorer allows two group-bys per request, so the tables are
// marginals. Every total is exact: usage cost per account and service
// comes from the account table and every non-usage record is its own line.
//
// A payer that covers one account takes its usage types from the usage
// table exactly and its teams per service from the team table; a service
// with several usage types and several teams has estimated cross cells.
//
// An Organization keeps account × service lines (stable keys; usage types
// are drill-down detail). Its team table is per account, so team totals per
// account are exact; a drilled pair takes team and region from its drill
// exactly; other services of an account share the account's remaining team
// totals, and only a cross cell with several services and several teams is
// estimated. Regions of undrilled lines follow the service's region mix and
// are estimated when that mix has more than one region.
//
// Splits keep both margins exact (each line's amount and each team total),
// and an estimated share is always flagged on the line.
func buildLines(day finops.Date, t *dayTables, o synthOptions) []finops.CostLine {
	mix := map[string][]cell{}
	for _, c := range t.Usage {
		mix[c.A] = append(mix[c.A], c)
	}
	accounts := map[string]bool{}
	for _, c := range t.Account {
		accounts[c.A] = true
	}
	single := !o.organization && len(accounts) <= 1
	teams := teamTotals(t.Team, o.keepTeams)

	byAccount := map[string][]cell{}
	for _, c := range t.Account {
		byAccount[c.A] = append(byAccount[c.A], c)
	}
	var out []finops.CostLine
	for _, account := range sortedKeys(byAccount) {
		rows := byAccount[account]
		sort.Slice(rows, func(i, j int) bool { return rows[i].B < rows[j].B })
		if single {
			out = append(out, singleLines(day, account, rows, mix, teams, t.TeamBy, o)...)
			continue
		}
		out = append(out, organizationLines(day, account, rows, mix, teams, t, o)...)
	}
	return append(out, recordLines(day, t, o)...)
}

func baseLine(day finops.Date, account, svc string, o synthOptions) finops.CostLine {
	return finops.CostLine{Day: day, Account: account, ServiceName: svc, RecordType: finops.RecordUsage, Currency: o.currency}
}

// singleLines builds one account's usage-type lines and splits each
// service's lines by that service's team totals.
func singleLines(day finops.Date, account string, rows []cell, mix map[string][]cell, teams map[string][]weight, teamBy string, o synthOptions) []finops.CostLine {
	var out []finops.CostLine
	for _, c := range rows {
		svc := c.B
		uts := append([]cell(nil), mix[svc]...)
		sort.Slice(uts, func(i, j int) bool { return uts[i].B < uts[j].B })
		var lines []finops.CostLine
		var sum int64
		for _, u := range uts {
			l := baseLine(day, account, svc, o)
			l.UsageType = u.B
			l.Service, l.UsageGroup = ServiceKey(svc, u.B)
			l.Region = RegionOf(svc, u.B)
			l.NetAmortizedMicros, l.QuantityMicros, l.Unit = u.M, u.Q, u.U
			sum += u.M
			lines = append(lines, l)
		}
		if rest := c.M - sum; rest != 0 || len(lines) == 0 {
			l := baseLine(day, account, svc, o)
			l.Service, _ = ServiceKey(svc, "")
			l.Region = RegionOf(svc, "")
			l.NetAmortizedMicros = rest
			lines = append(lines, l)
		}
		cols := teams[svc]
		if teamBy == teamByAccount {
			cols = teams[account]
		}
		out = append(out, splitTeams(lines, cols)...)
	}
	return out
}

// organizationLines builds one account's account × service lines.
func organizationLines(day finops.Date, account string, rows []cell, mix map[string][]cell, teams map[string][]weight, t *dayTables, o synthOptions) []finops.CostLine {
	var out, rest []finops.CostLine
	remaining := map[string]int64{}
	for _, w := range teams[account] {
		remaining[w.key] += w.w
	}
	for _, c := range rows {
		svc := c.B
		key, _ := ServiceKey(svc, "")
		drill, drilled := t.Drill[pairKey(account, svc)]
		var drillSum int64
		if drilled && o.detail {
			for _, d := range drill {
				team := ""
				if o.keepTeams {
					team = TagValue(d.B)
				}
				drillSum += d.M
				if t.TeamBy == teamByAccount {
					remaining[team] -= d.M
				}
				if d.M == 0 && d.Q == 0 {
					continue
				}
				l := baseLine(day, account, svc, o)
				l.Service, l.Region, l.Team, l.UsageType = key, RegionOf(svc, d.A), team, d.A
				l.NetAmortizedMicros, l.QuantityMicros, l.Unit = d.M, d.Q, d.U
				out = append(out, l)
			}
		} else if drilled {
			// Exact team and region for the pair from its drill-down.
			agg := map[[2]string]int64{}
			for _, d := range drill {
				team := ""
				if o.keepTeams {
					team = TagValue(d.B)
				}
				agg[[2]string{RegionOf(svc, d.A), team}] += d.M
				drillSum += d.M
				if t.TeamBy == teamByAccount {
					remaining[team] -= d.M
				}
			}
			for _, k := range sortedPairs(agg) {
				l := baseLine(day, account, svc, o)
				l.Service, l.Region, l.Team, l.NetAmortizedMicros = key, k[0], k[1], agg[k]
				if l.NetAmortizedMicros != 0 {
					out = append(out, l)
				}
			}
		}
		if amt := c.M - drillSum; amt != 0 {
			l := baseLine(day, account, svc, o)
			l.Service, l.NetAmortizedMicros = key, amt
			rest = append(rest, l)
		}
	}
	var cols []weight
	if t.TeamBy == teamByAccount {
		for _, k := range sortedKeys(remaining) {
			cols = append(cols, weight{k, remaining[k]})
		}
	} else {
		// Tables stored before the per-account team table: the account's
		// lines share each service's team mix, always as an estimate.
		var split []finops.CostLine
		for _, l := range rest {
			parts := splitTeams([]finops.CostLine{l}, teams[l.ServiceName])
			for i := range parts {
				parts[i].EstimatedTeam = len(parts) > 1
			}
			split = append(split, parts...)
		}
		return append(out, splitRegions(split, mix, t.regions)...)
	}
	return append(out, splitRegions(splitTeams(rest, cols), mix, t.regions)...)
}

// splitTeams splits lines by team totals, both margins exact when they
// agree. Cross cells are flagged estimated when several lines share
// several teams.
func splitTeams(lines []finops.CostLine, cols []weight) []finops.CostLine {
	var pos []weight
	for _, w := range cols {
		if w.w > 0 {
			pos = append(pos, w)
		}
	}
	if len(pos) == 0 {
		if len(cols) == 1 {
			for i := range lines {
				lines[i].Team = cols[0].key
			}
		}
		return lines
	}
	amounts := make([]int64, len(lines))
	for i, l := range lines {
		amounts[i] = l.NetAmortizedMicros
	}
	cells := allocate(amounts, pos)
	estimated := len(pos) > 1 && len(lines) > 1
	var out []finops.CostLine
	for i, l := range lines {
		var qtyW []weight
		for j, w := range pos {
			qtyW = append(qtyW, weight{w.key, max(cells[i][j], 0)})
		}
		qty := splitWeights(0, l.QuantityMicros, qtyW)
		for j, w := range pos {
			c := l
			c.Team, c.NetAmortizedMicros = w.key, cells[i][j]
			c.QuantityMicros = 0
			for _, q := range qty {
				if q.key == w.key {
					c.QuantityMicros = q.qty
				}
			}
			c.EstimatedTeam = l.EstimatedTeam || (estimated && c.NetAmortizedMicros != 0)
			if c.NetAmortizedMicros != 0 || c.QuantityMicros != 0 {
				out = append(out, c)
			}
		}
	}
	return out
}

// splitRegions gives each line a region from its service's usage mix, or
// from the given region mix when there is one.
func splitRegions(lines []finops.CostLine, mix map[string][]cell, regions map[string][]weight) []finops.CostLine {
	var out []finops.CostLine
	for _, l := range lines {
		if isGlobalService(l.ServiceName) {
			l.Region = finops.GlobalRegion
			out = append(out, l)
			continue
		}
		ws := regionWeights(l.ServiceName, mix[l.ServiceName])
		if regions != nil {
			ws = regions[l.ServiceName]
			if len(ws) == 0 {
				ws = []weight{{finops.UnknownRegion, 1}}
			}
		}
		parts := splitWeights(l.NetAmortizedMicros, l.QuantityMicros, ws)
		for _, p := range parts {
			r := l
			r.Region, r.NetAmortizedMicros, r.QuantityMicros = p.key, p.amount, p.qty
			r.EstimatedRegion = len(parts) > 1
			if r.Region == "" {
				r.Region = finops.UnknownRegion
			}
			if r.NetAmortizedMicros != 0 || r.QuantityMicros != 0 {
				out = append(out, r)
			}
		}
	}
	return out
}

// recordLines makes every non-usage record its own line.
func recordLines(day finops.Date, t *dayTables, o synthOptions) []finops.CostLine {
	rec := append([]cell(nil), t.Record...)
	sort.Slice(rec, func(i, j int) bool {
		if rec[i].B != rec[j].B {
			return rec[i].B < rec[j].B
		}
		return rec[i].A < rec[j].A
	})
	var out []finops.CostLine
	for _, c := range rec {
		if isUsageRecord(c.A) || c.M == 0 {
			continue
		}
		record, svc := recordLine(c.A)
		out = append(out, finops.CostLine{
			Day: day, Account: c.B, Region: finops.GlobalRegion, Service: svc, ServiceName: c.A,
			UsageType: c.A, RecordType: record, Currency: o.currency, NetAmortizedMicros: c.M,
		})
	}
	return out
}

// buildResourceLines turns resource detail into refinement lines. The usage
// type names the service through the day's usage table.
func buildResourceLines(day finops.Date, t *dayTables, o synthOptions) []finops.CostLine {
	svcOf := map[string]string{}
	for _, c := range t.Usage {
		if _, ok := svcOf[c.B]; !ok {
			svcOf[c.B] = c.A
		}
	}
	var out []finops.CostLine
	for _, c := range t.Resource {
		svc := svcOf[c.A]
		if svc == "" || c.B == "" {
			continue
		}
		key, group := ServiceKey(svc, c.A)
		out = append(out, finops.CostLine{
			Day: day, Account: o.payer, Region: RegionOf(svc, c.A), Service: key, ServiceName: svc,
			UsageType: c.A, UsageGroup: group, RecordType: finops.RecordUsage, ResourceID: c.B,
			Currency: o.currency, NetAmortizedMicros: c.M, QuantityMicros: c.Q, Unit: c.U,
		})
	}
	return out
}

// buildDrillLines turns an Organization drill-down into exact usage-type
// lines with their team for one account and service.
func buildDrillLines(day finops.Date, account, svc string, cells []cell, o synthOptions) []finops.CostLine {
	var out []finops.CostLine
	for _, c := range cells {
		key, group := ServiceKey(svc, c.A)
		l := finops.CostLine{
			Day: day, Account: account, Region: RegionOf(svc, c.A), Service: key, ServiceName: svc,
			UsageType: c.A, UsageGroup: group, RecordType: finops.RecordUsage, Currency: o.currency,
			NetAmortizedMicros: c.M, QuantityMicros: c.Q, Unit: c.U,
		}
		if o.keepTeams {
			l.Team = TagValue(c.B)
		}
		out = append(out, l)
	}
	return out
}

type weight struct {
	key string
	w   int64
}

// teamTotals groups the team table by its second key (service or
// account). Without an allowlisted tag every value becomes "" so no tag
// value reaches a view.
func teamTotals(cells []cell, keep bool) map[string][]weight {
	sum := map[string]map[string]int64{}
	for _, c := range cells {
		v := ""
		if keep {
			v = TagValue(c.A)
		}
		if sum[c.B] == nil {
			sum[c.B] = map[string]int64{}
		}
		sum[c.B][v] += c.M
	}
	out := map[string][]weight{}
	for k, m := range sum {
		for _, v := range sortedKeys(m) {
			out[k] = append(out[k], weight{v, m[v]})
		}
	}
	return out
}

// regionWeights is the service's cost by region from its usage types.
func regionWeights(svc string, uts []cell) []weight {
	m := map[string]int64{}
	for _, u := range uts {
		m[RegionOf(svc, u.B)] += u.M
	}
	var out []weight
	for _, r := range sortedKeys(m) {
		out = append(out, weight{r, m[r]})
	}
	if len(out) == 0 {
		out = []weight{{finops.UnknownRegion, 1}}
	}
	return out
}

type part struct {
	key    string
	amount int64
	qty    int64
}

// splitWeights splits an amount and a quantity by positive weights with
// largest remainders, so the parts sum exactly. With no positive weight the
// whole amount goes to the single key, or to "" when there is none.
func splitWeights(amount, qty int64, ws []weight) []part {
	var pos []weight
	var total int64
	for _, w := range ws {
		if w.w > 0 {
			pos = append(pos, w)
			total += w.w
		}
	}
	if len(pos) == 0 {
		key := ""
		if len(ws) == 1 {
			key = ws[0].key
		}
		return []part{{key: key, amount: amount, qty: qty}}
	}
	if len(pos) == 1 {
		return []part{{key: pos[0].key, amount: amount, qty: qty}}
	}
	amounts := largestRemainder(amount, pos, total)
	qtys := largestRemainder(qty, pos, total)
	out := make([]part, len(pos))
	for i, w := range pos {
		out[i] = part{key: w.key, amount: amounts[i], qty: qtys[i]}
	}
	return out
}

// allocate splits row amounts across positive column totals. Each row
// sums exactly to its amount; when the rows and columns have the same
// total, each column sums exactly to its total too (rounding is moved
// between cells of a row, never created).
func allocate(rows []int64, cols []weight) [][]int64 {
	var total, rowSum int64
	for _, w := range cols {
		total += w.w
	}
	out := make([][]int64, len(rows))
	for i, r := range rows {
		out[i] = largestRemainder(r, cols, total)
		rowSum += r
	}
	if rowSum != total {
		return out
	}
	for _, r := range rows {
		if r < 0 {
			return out
		}
	}
	deficit := make([]int64, len(cols))
	for j, w := range cols {
		deficit[j] = w.w
		for i := range rows {
			deficit[j] -= out[i][j]
		}
	}
	for p := range cols {
		for n := range cols {
			for i := range rows {
				if deficit[p] <= 0 {
					break
				}
				if deficit[n] >= 0 || out[i][n] <= 0 {
					continue
				}
				m := min(deficit[p], -deficit[n], out[i][n])
				out[i][n] -= m
				out[i][p] += m
				deficit[n] += m
				deficit[p] -= m
			}
		}
	}
	return out
}

func largestRemainder(amount int64, ws []weight, total int64) []int64 {
	neg := amount < 0
	a := new(big.Int).Abs(big.NewInt(amount))
	t := big.NewInt(total)
	out := make([]int64, len(ws))
	rems := make([]*big.Int, len(ws))
	var given int64
	for i, w := range ws {
		q, r := new(big.Int).QuoRem(new(big.Int).Mul(a, big.NewInt(w.w)), t, new(big.Int))
		out[i] = q.Int64()
		rems[i] = r
		given += out[i]
	}
	idx := make([]int, len(ws))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		if c := rems[idx[x]].Cmp(rems[idx[y]]); c != 0 {
			return c > 0
		}
		return ws[idx[x]].key < ws[idx[y]].key
	})
	for left, k := a.Int64()-given, 0; left > 0; left, k = left-1, k+1 {
		out[idx[k%len(idx)]]++
	}
	if neg {
		for i := range out {
			out[i] = -out[i]
		}
	}
	return out
}

// pairKey names an account and service in drill-down maps.
func pairKey(account, service string) string { return account + "|" + service }

func splitPair(k string) (string, string) {
	a, s, _ := strings.Cut(k, "|")
	return a, s
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPairs(m map[[2]string]int64) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
