package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// meterSwapAttempts bounds the compare-and-swap retries of one Take. Each
// lost attempt means another replica stored a new count in between.
const meterSwapAttempts = 16

// errMeterContended means the day's count kept changing under other
// replicas, so no request was reserved and none may be sent. It is an
// ErrBusy: the caller answers refresh_in_progress and may retry shortly.
var errMeterContended = fmt.Errorf("%w: paid-request meter contended; request not sent", ErrBusy)

// Meter is the deployment's paid-request meter. It persists each day's count
// and refuses any request past the cap, so the cap is enforced in one place
// whoever calls. It implements finops.Meter.
type Meter struct {
	cache cache
	cap   func() int
	today func() finops.Date
	now   func() time.Time

	mu     sync.Mutex
	day    finops.Date
	used   int
	loaded bool
}

// meterRecord is one day's use.
type meterRecord struct {
	Day       string    `json:"day"`
	Requests  int       `json:"requests"`
	Dollars   int64     `json:"dollars_micros"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newMeter(c cache, capRequests func() int, today func() finops.Date, now func() time.Time) *Meter {
	return &Meter{cache: c, cap: capRequests, today: today, now: now}
}

// sync loads the day's count, rolling over at midnight in the deployment
// time zone. The caller holds the lock. The stored count is read every time:
// another replica sharing the storage may have spent requests since this
// process last looked, and the cap is for the deployment, not the process.
func (m *Meter) sync() error {
	today := m.today()
	rec := meterRecord{}
	if _, err := m.cache.read(func(org string) string { return meterKey(org, today.String()) }, &rec); err != nil {
		return err
	}
	m.roll(today, rec.Requests)
	return nil
}

// roll takes the stored count of today as the meter's use, never below what
// this process already counted today, and drops the record that left the
// retention when the day changed. The caller holds the lock.
func (m *Meter) roll(today finops.Date, stored int) {
	used := stored
	if m.loaded && m.day == today {
		used = max(used, m.used)
	}
	if m.loaded && m.day != today {
		_ = m.cache.remove(meterKey(m.cache.scope.Write, today.AddDays(-meterRetentionDays).String()))
	}
	m.day, m.used, m.loaded = today, used, true
}

// Take reserves n requests, or returns finops.ErrCapReached and takes none.
// The count is stored before the request is sent; a request whose count
// cannot be stored is not sent.
//
// On a backend with compare-and-swap the new count is stored only over the
// count it was computed from; a lost race reads again, so replicas sharing
// the storage never reserve the same requests twice and the cap holds even
// without the replica lock. A backend without it serves one process, where
// the meter's lock is enough.
func (m *Meter) Take(_ context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cas, ok := m.cache.provider.(storage.BlobCAS)
	if !ok {
		return m.takeStored(n)
	}
	for range meterSwapAttempts {
		stored, err := m.load()
		if err != nil {
			return err
		}
		if m.used+n > m.cap() {
			return finops.ErrCapReached
		}
		rec := m.record(m.used + n)
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		swapped, err := cas.CompareAndSwapBlob(meterKey(m.cache.scope.Write, rec.Day), stored, data)
		if err != nil {
			return err
		}
		if swapped {
			m.used += n
			return nil
		}
	}
	return errMeterContended
}

// takeStored is Take on a backend without compare-and-swap: read, check and
// write under the meter's lock.
func (m *Meter) takeStored(n int) error {
	if err := m.sync(); err != nil {
		return err
	}
	if m.used+n > m.cap() {
		return finops.ErrCapReached
	}
	rec := m.record(m.used + n)
	if err := m.cache.write(func(org string) string { return meterKey(org, rec.Day) }, rec); err != nil {
		return err
	}
	m.used += n
	return nil
}

// record is today's stored use at the given count.
func (m *Meter) record(requests int) meterRecord {
	return meterRecord{Day: m.day.String(), Requests: requests, Dollars: int64(requests) * requestPriceMicros, UpdatedAt: m.now()}
}

// load syncs the day's count like sync and returns the write
// organization's stored bytes it was read from, the expected value of the
// next swap (nil when the day has no record there yet). The caller holds
// the lock.
func (m *Meter) load() ([]byte, error) {
	today := m.today()
	stored, err := m.cache.provider.ReadBlob(meterKey(m.cache.scope.Write, today.String()))
	if err != nil {
		return nil, err
	}
	rec := meterRecord{}
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &rec); err != nil {
			return nil, err
		}
	} else {
		stored = nil
		if _, err := m.cache.read(func(org string) string { return meterKey(org, today.String()) }, &rec); err != nil {
			return nil, err
		}
	}
	m.roll(today, rec.Requests)
	return stored, nil
}

// Used reports today's requests.
func (m *Meter) Used(context.Context) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.sync(); err != nil {
		return m.used
	}
	return m.used
}

// Cap reports the daily cap.
func (m *Meter) Cap() int { return m.cap() }

// Remaining reports the requests left today.
func (m *Meter) Remaining(ctx context.Context) int { return max(0, m.Cap()-m.Used(ctx)) }
