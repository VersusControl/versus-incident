package finops

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"sort"
)

// Causes of a cost change. The strings are part of the API contract.
const (
	CauseNewResource     = "new_resource"
	CauseRemovedResource = "removed_resource"
	CauseUsageUp         = "usage_up"
	CauseUsageDown       = "usage_down"
	CausePriceChange     = "price_change"
	CauseDiscountChange  = "discount_change"
	CauseOneOff          = "one_off"
	CauseNewAccount      = "new_account"
	CauseOther           = "other"
)

// Driver rule constants.
const (
	// spikeFactor: a single day above this multiple of the line's median
	// day is a one-time charge, not a trend.
	spikeFactor = 5
	// spikeMinDays: the spike rule needs this many charged days to have a
	// meaningful median.
	spikeMinDays = 3
	// priceToleranceBP: unit price moves inside ±1% are rounding noise and
	// count as usage.
	priceToleranceBP = 100
	// DefaultMinDriver rolls smaller drivers into "Everything else".
	DefaultMinDriver = 50 * MicrosPerUnit
	// DefaultMaxDrivers caps the bridge rows; the rest roll into
	// "Everything else".
	DefaultMaxDrivers = 12
)

// BridgeOptions tune the driver split.
type BridgeOptions struct {
	MinDriver  Micros // |amount| below this rolls into "Everything else"
	MaxDrivers int
}

func (o BridgeOptions) withDefaults() BridgeOptions {
	if o.MinDriver <= 0 {
		o.MinDriver = DefaultMinDriver
	}
	if o.MaxDrivers <= 0 {
		o.MaxDrivers = DefaultMaxDrivers
	}
	return o
}

// Driver is one cause of the change on one line (or one new account).
type Driver struct {
	ID          string  `json:"id"`
	Cause       string  `json:"cause"`
	Key         LineKey `json:"key"`
	ServiceName string  `json:"service_name,omitempty"`
	Team        string  `json:"team,omitempty"`
	// TeamEstimated marks a team that comes from an estimated share.
	TeamEstimated bool   `json:"team_estimated,omitempty"`
	Amount        Micros `json:"amount_micros"`
	// Since is the first period day the line (or account) had cost, for
	// new resources and new accounts.
	Since Date `json:"since,omitzero"`
	// Lines is the number of lines rolled in (the "other" row, new accounts).
	Lines int `json:"lines,omitempty"`
}

// Bridge is the split of the change between the base and the period. The
// invariant Base + Σ Drivers + Other.Amount == Total holds exactly.
type Bridge struct {
	Base    Micros   `json:"base_micros"`
	Total   Micros   `json:"total_micros"`
	Change  Micros   `json:"change_micros"`
	Drivers []Driver `json:"drivers"`
	Other   Driver   `json:"other"`
	// Up and Down are the sums of every positive and negative piece before
	// the roll-up; they show how much offset.
	Up   Micros `json:"up_micros"`
	Down Micros `json:"down_micros"`
	// Steady is true when no driver passes the minimum.
	Steady bool `json:"steady"`
}

type lineAgg struct {
	key         LineKey
	serviceName string
	teams       teamShare
	cost        Micros
	qty         int64
	days        map[Date]Micros
	first       Date
}

func aggregate(lines []CostLine, r DateRange, keepDays bool) map[LineKey]*lineAgg {
	out := map[LineKey]*lineAgg{}
	for _, l := range lines {
		if !r.Contains(l.Day) {
			continue
		}
		k := l.Key()
		a := out[k]
		if a == nil {
			a = &lineAgg{key: k, first: l.Day}
			if keepDays {
				a.days = map[Date]Micros{}
			}
			out[k] = a
		}
		a.cost += l.NetAmortizedMicros
		a.qty += l.QuantityMicros
		if l.ServiceName != "" {
			a.serviceName = l.ServiceName
		}
		a.teams.add(l)
		if l.Day.Before(a.first) {
			a.first = l.Day
		}
		if keepDays {
			a.days[l.Day] += l.NetAmortizedMicros
		}
	}
	return out
}

func (a *lineAgg) present() bool { return a != nil && (a.cost != 0 || a.qty != 0) }

// ComputeBridge splits Δ = cost(period) − cost(base) into causes.
// Lines outside both ranges are ignored. Callers pass complete days only.
func ComputeBridge(lines []CostLine, period, base DateRange, opts BridgeOptions) Bridge {
	opts = opts.withDefaults()
	pAgg := aggregate(lines, period, true)
	bAgg := aggregate(lines, base, false)

	var b Bridge
	for _, a := range pAgg {
		b.Total += a.cost
	}
	for _, a := range bAgg {
		b.Base += a.cost
	}
	b.Change = b.Total - b.Base

	// Accounts with cost in the period and none in the base are one
	// "new account" driver each, not a row per line.
	baseAccounts := map[string]bool{}
	for k, a := range bAgg {
		if a.present() {
			baseAccounts[k.Account] = true
		}
	}

	keys := make([]LineKey, 0, len(pAgg)+len(bAgg))
	seen := map[LineKey]bool{}
	for _, m := range []map[LineKey]*lineAgg{pAgg, bAgg} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })

	var pieces []Driver
	newAccounts := map[string]*Driver{}
	for _, k := range keys {
		p, bs := pAgg[k], bAgg[k]
		if !baseAccounts[k.Account] && p.present() {
			na := newAccounts[k.Account]
			if na == nil {
				na = &Driver{Cause: CauseNewAccount, Key: LineKey{Account: k.Account}, Since: p.first}
				newAccounts[k.Account] = na
			}
			na.Amount += p.cost
			na.Lines++
			if p.first.Before(na.Since) {
				na.Since = p.first
			}
			continue
		}
		pieces = append(pieces, splitLine(k, p, bs)...)
	}
	for _, acct := range sortedKeys(newAccounts) {
		if d := newAccounts[acct]; d.Amount != 0 {
			pieces = append(pieces, *d)
		}
	}

	for _, d := range pieces {
		if d.Amount > 0 {
			b.Up += d.Amount
		} else {
			b.Down += d.Amount
		}
	}

	b.Other = Driver{Cause: CauseOther, ID: driverID(CauseOther, LineKey{})}
	var kept []Driver
	for _, d := range pieces {
		d.ID = driverID(d.Cause, d.Key)
		if absMicros(d.Amount) < opts.MinDriver {
			b.Other.Amount += d.Amount
			b.Other.Lines++
			continue
		}
		kept = append(kept, d)
	}
	b.Steady = len(kept) == 0
	sort.SliceStable(kept, func(i, j int) bool {
		ai, aj := absMicros(kept[i].Amount), absMicros(kept[j].Amount)
		if ai != aj {
			return ai > aj
		}
		return kept[i].ID < kept[j].ID
	})
	if len(kept) > opts.MaxDrivers {
		for _, d := range kept[opts.MaxDrivers:] {
			b.Other.Amount += d.Amount
			b.Other.Lines++
		}
		kept = kept[:opts.MaxDrivers]
	}
	orderForBridge(kept, b.Change)
	b.Drivers = kept
	return b
}

// splitLine applies the cause rules to one line key.
func splitLine(k LineKey, p, bs *lineAgg) []Driver {
	var pc, bc Micros
	var pq, bq int64
	var name, team string
	var teamEst bool
	var since Date
	if p != nil {
		pc, pq, name, since = p.cost, p.qty, p.serviceName, p.first
		team, teamEst = p.teams.top()
	}
	if bs != nil {
		bc, bq = bs.cost, bs.qty
		if name == "" {
			name = bs.serviceName
		}
		if p == nil {
			team, teamEst = bs.teams.top()
		}
	}
	delta := pc - bc
	mk := func(cause string, amt Micros) Driver {
		return Driver{Cause: cause, Key: k, ServiceName: name, Team: team, TeamEstimated: teamEst, Amount: amt}
	}
	switch k.RecordType {
	case RecordFee, RecordRefund, RecordTax:
		return nonZero(mk(CauseOneOff, delta))
	case RecordDiscount, RecordCredit:
		return nonZero(mk(CauseDiscountChange, delta))
	}
	switch {
	case p.present() && !bs.present():
		d := mk(CauseNewResource, delta)
		d.Since = since
		return nonZero(d)
	case !p.present() && bs.present():
		return nonZero(mk(CauseRemovedResource, delta))
	case !p.present() && !bs.present():
		return nonZero(mk(usageCause(delta), delta))
	}

	var out []Driver
	// A single-day charge far above the line's usual day is one-time.
	if spike := spikeExcess(p.days); spike > 0 {
		out = append(out, mk(CauseOneOff, spike))
		delta -= spike
		pc -= spike
	}
	usage := delta
	if pq > 0 && bq > 0 && priceMoved(pc, pq, bc, bq) {
		usage = mulDiv(pq-bq, bc, bq) // Δq × base unit price
		if price := delta - usage; price != 0 {
			out = append(out, mk(CausePriceChange, price))
		}
	}
	out = append(out, nonZero(mk(usageCause(usage), usage))...)
	return out
}

func usageCause(amt Micros) string {
	if amt < 0 {
		return CauseUsageDown
	}
	return CauseUsageUp
}

func nonZero(d Driver) []Driver {
	if d.Amount == 0 {
		return nil
	}
	return []Driver{d}
}

// priceMoved compares unit prices pc/pq and bc/bq by cross-multiplying, so
// no division rounding hides or invents a move.
func priceMoved(pc, pq, bc, bq int64) bool {
	lhs := new(big.Int).Mul(big.NewInt(pc), big.NewInt(bq)) // pc·bq
	rhs := new(big.Int).Mul(big.NewInt(bc), big.NewInt(pq)) // bc·pq
	diff := new(big.Int).Sub(lhs, rhs)
	diff.Abs(diff).Mul(diff, big.NewInt(10_000))
	tol := new(big.Int).Abs(rhs)
	tol.Mul(tol, big.NewInt(priceToleranceBP))
	return diff.Cmp(tol) > 0
}

// spikeExcess returns how much the line's largest day exceeds its median
// charged day, when that day is more than spikeFactor× the median.
func spikeExcess(days map[Date]Micros) Micros {
	var vals []int64
	for _, v := range days {
		if v > 0 {
			vals = append(vals, v)
		}
	}
	if len(vals) < spikeMinDays {
		return 0
	}
	med := median(vals)
	mx := vals[0]
	for _, v := range vals {
		mx = max(mx, v)
	}
	if med > 0 && mx > spikeFactor*med {
		return mx - med
	}
	return 0
}

func orderForBridge(ds []Driver, change Micros) {
	upFirst := change >= 0
	sort.SliceStable(ds, func(i, j int) bool {
		si, sj := ds[i].Amount > 0, ds[j].Amount > 0
		if si != sj {
			return si == upFirst
		}
		ai, aj := absMicros(ds[i].Amount), absMicros(ds[j].Amount)
		if ai != aj {
			return ai > aj
		}
		return ds[i].ID < ds[j].ID
	})
}

func driverID(cause string, k LineKey) string {
	return shortHash("driver", cause, k.String())
}

func shortHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func absMicros(v Micros) Micros {
	if v < 0 {
		return -v
	}
	return v
}

// median of a non-empty slice; it sorts a copy. Even counts average the
// middle pair, rounded half away from zero.
func median(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]int64(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return mulDiv(s[n/2-1]+s[n/2], 1, 2)
}
