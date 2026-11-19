package suspect

import (
	"context"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// ChangeReader is the snapshot store's change read.
type ChangeReader interface {
	Changes(ctx context.Context, q snapshot.Query, since, until time.Time) (snapshot.ChangesResult, error)
}

// Snapshot serves both the changes and their coverage from one snapshot
// read per window, so the rows and the completeness answer come from the
// same read. A window that was never read is not settled.
type Snapshot struct {
	reader ChangeReader
	mu     sync.Mutex
	read   map[window]bool
}

type window struct {
	account, region string
	from, to        time.Time
}

// NewSnapshot wraps the snapshot store as a ChangeSource and Coverage.
func NewSnapshot(reader ChangeReader) *Snapshot {
	return &Snapshot{reader: reader, read: map[window]bool{}}
}

// Changes implements ChangeSource.
func (s *Snapshot) Changes(ctx context.Context, account, region string, from, to time.Time) ([]model.Change, error) {
	result, err := s.reader.Changes(ctx, snapshot.Query{Account: account, Region: region}, from, to)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.read[window{account, region, from.UTC(), to.UTC()}] = result.Complete
	s.mu.Unlock()
	return result.Changes, nil
}

// Settled implements Coverage. The snapshot store claims coverage only up
// to the settled poll boundary, so Complete means read and settled.
func (s *Snapshot) Settled(account, region string, from, to time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read[window{account, region, from.UTC(), to.UTC()}]
}
