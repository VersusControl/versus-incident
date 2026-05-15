// Package teams provides CRUD over operator-defined members and teams.
//
// Members and teams are persisted as two opaque blobs ("members" and
// "teams") through storage.Provider, so whichever backend the rest of
// the service uses (file / redis / database) automatically applies. No
// extra config is required.
//
// The store is purely a directory of identities — it does not (yet)
// participate in alert routing. Phase-2 work will read the per-member
// meta map (slack_id, telegram_id, ...) to choose channels per
// assignment.
package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/google/uuid"
)

// Blob names used through storage.Provider.
const (
	BlobMembers = "members"
	BlobTeams   = "teams"
)

// ErrNotFound is returned by Get/Update/Delete when the id is unknown.
var ErrNotFound = errors.New("teams: not found")

// ErrInvalid is returned for validation failures (empty name, etc).
var ErrInvalid = errors.New("teams: invalid input")

// Member is an operator-defined person who can be assigned to incidents.
//
// Alias is auto-derived from Name on first save when empty, but the UI
// allows operators to edit it independently afterwards.
//
// Meta carries typed per-channel identifiers. Each field is optional;
// only set the ones the operator actually uses. Routing logic (Phase 2)
// reads these to pick channels per assignee — the store itself does
// not interpret them.
type Member struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Alias     string     `json:"alias"`
	Meta      MemberMeta `json:"meta"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// MemberMeta holds per-channel identifiers for a member. Fields are
// matched to the channels Versus already ships with; add a new field
// here (and to clone/equal helpers) when a new channel lands.
type MemberMeta struct {
	// Email address, also used for the SMTP/email channel.
	Email string `json:"email,omitempty"`
	// Slack member id (e.g. "U0123ABC"), NOT the @handle.
	SlackID string `json:"slack_id,omitempty"`
	// Telegram numeric user id as a string.
	TelegramID string `json:"telegram_id,omitempty"`
	// Microsoft Teams user principal name (UPN), typically the work email.
	MSTeamsUPN string `json:"msteams_upn,omitempty"`
	// Viber user id (channel API) for direct messaging.
	ViberID string `json:"viber_id,omitempty"`
	// Lark / Feishu open_id or union_id for direct messaging.
	LarkID string `json:"lark_id,omitempty"`
	// PagerDuty user id (used by future routing to attach assignees).
	PagerDutyUserID string `json:"pagerduty_user_id,omitempty"`
	// AWS Incident Manager contact ARN.
	AWSIMContactARN string `json:"awsim_contact_arn,omitempty"`
	// Phone number in E.164 (for future SMS / voice channels).
	Phone string `json:"phone,omitempty"`
}

// Team is an ordered group of member IDs.
type Team struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Alias       string    `json:"alias"`
	Description string    `json:"description,omitempty"`
	MemberIDs   []string  `json:"member_ids"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store is the in-process registry of members and teams. It loads
// state lazily from storage on first use and persists eagerly after
// every mutation. All exported methods are safe for concurrent use.
type Store struct {
	provider storage.Provider

	mu      sync.RWMutex
	members map[string]*Member
	teams   map[string]*Team
	loaded  bool
}

// NewStore returns a Store backed by the given storage provider.
// Provider must be non-nil.
func NewStore(p storage.Provider) (*Store, error) {
	if p == nil {
		return nil, fmt.Errorf("teams: nil storage provider")
	}
	s := &Store{
		provider: p,
		members:  make(map[string]*Member),
		teams:    make(map[string]*Team),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// --- persistence -----------------------------------------------------------

type membersFile struct {
	Version   int                `json:"version"`
	UpdatedAt time.Time          `json:"updated_at"`
	Members   map[string]*Member `json:"members"`
}

type teamsFile struct {
	Version   int              `json:"version"`
	UpdatedAt time.Time        `json:"updated_at"`
	Teams     map[string]*Team `json:"teams"`
}

const schemaVersion = 1

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	s.loaded = true

	if data, err := s.provider.ReadBlob(BlobMembers); err != nil {
		return fmt.Errorf("teams: read members blob: %w", err)
	} else if len(data) > 0 {
		var f membersFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("teams: parse members blob: %w", err)
		}
		if f.Members != nil {
			s.members = f.Members
		}
	}
	if data, err := s.provider.ReadBlob(BlobTeams); err != nil {
		return fmt.Errorf("teams: read teams blob: %w", err)
	} else if len(data) > 0 {
		var f teamsFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("teams: parse teams blob: %w", err)
		}
		if f.Teams != nil {
			s.teams = f.Teams
		}
	}
	return nil
}

func (s *Store) persistMembersLocked() error {
	data, err := json.MarshalIndent(membersFile{
		Version:   schemaVersion,
		UpdatedAt: time.Now().UTC(),
		Members:   s.members,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("teams: marshal members: %w", err)
	}
	return s.provider.WriteBlob(BlobMembers, data)
}

func (s *Store) persistTeamsLocked() error {
	data, err := json.MarshalIndent(teamsFile{
		Version:   schemaVersion,
		UpdatedAt: time.Now().UTC(),
		Teams:     s.teams,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("teams: marshal teams: %w", err)
	}
	return s.provider.WriteBlob(BlobTeams, data)
}

// --- members ---------------------------------------------------------------

// DeriveAlias produces a URL-safe lowercase slug from a human name.
// Mirrors the auto-fill the UI applies before the operator edits it.
// Exported for the controller's PATCH behavior and tests.
func DeriveAlias(name string) string {
	var b strings.Builder
	prevDash := true // suppress leading dashes
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	return out
}

// CreateMember inserts a new member. ID is server-generated. Alias is
// derived from Name when blank.
func (s *Store) CreateMember(in Member) (*Member, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: member name required", ErrInvalid)
	}
	now := time.Now().UTC()
	m := &Member{
		ID:        uuid.NewString(),
		Name:      strings.TrimSpace(in.Name),
		Alias:     strings.TrimSpace(in.Alias),
		Meta:      in.Meta,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if m.Alias == "" {
		m.Alias = DeriveAlias(m.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[m.ID] = m
	if err := s.persistMembersLocked(); err != nil {
		delete(s.members, m.ID)
		return nil, err
	}
	return cloneMember(m), nil
}

// UpdateMember applies a partial patch. Empty strings on Name/Alias are
// treated as "no change". Pass replaceMeta=true to swap the entire
// MemberMeta struct (the zero value clears every channel id);
// replaceMeta=false leaves the existing meta untouched.
func (s *Store) UpdateMember(id string, patch Member, replaceMeta bool) (*Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[id]
	if !ok {
		return nil, ErrNotFound
	}
	if v := strings.TrimSpace(patch.Name); v != "" {
		m.Name = v
	}
	if v := strings.TrimSpace(patch.Alias); v != "" {
		m.Alias = v
	}
	if replaceMeta {
		m.Meta = patch.Meta
	}
	m.UpdatedAt = time.Now().UTC()
	if err := s.persistMembersLocked(); err != nil {
		return nil, err
	}
	return cloneMember(m), nil
}

// DeleteMember removes a member. Returns ErrNotFound when unknown.
// Any team referencing the id keeps a dangling reference — callers are
// responsible for cleaning teams (the controller does this).
func (s *Store) DeleteMember(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.members[id]; !ok {
		return ErrNotFound
	}
	delete(s.members, id)
	// Also strip the id from every team's MemberIDs to avoid dangling refs.
	teamsDirty := false
	for _, t := range s.teams {
		filtered := t.MemberIDs[:0]
		for _, mid := range t.MemberIDs {
			if mid != id {
				filtered = append(filtered, mid)
			}
		}
		if len(filtered) != len(t.MemberIDs) {
			t.MemberIDs = append([]string(nil), filtered...)
			t.UpdatedAt = time.Now().UTC()
			teamsDirty = true
		}
	}
	if err := s.persistMembersLocked(); err != nil {
		return err
	}
	if teamsDirty {
		if err := s.persistTeamsLocked(); err != nil {
			return err
		}
	}
	return nil
}

// GetMember returns a deep copy of the member or ErrNotFound.
func (s *Store) GetMember(id string) (*Member, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.members[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneMember(m), nil
}

// ListMembers returns members sorted by Name (case-insensitive).
func (s *Store) ListMembers() []*Member {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Member, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, cloneMember(m))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// --- teams -----------------------------------------------------------------

// CreateTeam inserts a new team. MemberIDs are validated against the
// known member set; unknown ids are rejected as ErrInvalid.
func (s *Store) CreateTeam(in Team) (*Team, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: team name required", ErrInvalid)
	}
	now := time.Now().UTC()
	t := &Team{
		ID:          uuid.NewString(),
		Name:        strings.TrimSpace(in.Name),
		Alias:       strings.TrimSpace(in.Alias),
		Description: strings.TrimSpace(in.Description),
		MemberIDs:   append([]string(nil), in.MemberIDs...),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if t.Alias == "" {
		t.Alias = DeriveAlias(t.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateMemberIDsLocked(t.MemberIDs); err != nil {
		return nil, err
	}
	s.teams[t.ID] = t
	if err := s.persistTeamsLocked(); err != nil {
		delete(s.teams, t.ID)
		return nil, err
	}
	return cloneTeam(t), nil
}

// UpdateTeam applies a partial patch. Pass replaceMembers=true to
// replace the member list (empty list clears it); false leaves the
// existing list alone.
func (s *Store) UpdateTeam(id string, patch Team, replaceMembers bool) (*Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.teams[id]
	if !ok {
		return nil, ErrNotFound
	}
	if v := strings.TrimSpace(patch.Name); v != "" {
		t.Name = v
	}
	if v := strings.TrimSpace(patch.Alias); v != "" {
		t.Alias = v
	}
	// Description: empty string is a valid clear, so only patch when
	// the caller actually sent the field. The controller signals
	// "field present" by passing the patch verbatim; we update
	// unconditionally and treat blank as "leave alone" to keep the
	// API simple.
	if v := strings.TrimSpace(patch.Description); v != "" {
		t.Description = v
	}
	if replaceMembers {
		if err := s.validateMemberIDsLocked(patch.MemberIDs); err != nil {
			return nil, err
		}
		t.MemberIDs = append([]string(nil), patch.MemberIDs...)
	}
	t.UpdatedAt = time.Now().UTC()
	if err := s.persistTeamsLocked(); err != nil {
		return nil, err
	}
	return cloneTeam(t), nil
}

// DeleteTeam removes a team. Returns ErrNotFound when unknown.
func (s *Store) DeleteTeam(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teams[id]; !ok {
		return ErrNotFound
	}
	delete(s.teams, id)
	return s.persistTeamsLocked()
}

// GetTeam returns a deep copy of the team or ErrNotFound.
func (s *Store) GetTeam(id string) (*Team, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.teams[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneTeam(t), nil
}

// ListTeams returns teams sorted by Name (case-insensitive).
func (s *Store) ListTeams() []*Team {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Team, 0, len(s.teams))
	for _, t := range s.teams {
		out = append(out, cloneTeam(t))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// ValidateMemberIDs is the public flavor of validateMemberIDsLocked,
// used by the incident-assignment controller.
func (s *Store) ValidateMemberIDs(ids []string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.validateMemberIDsLocked(ids)
}

// TeamExists reports whether a team id is known.
func (s *Store) TeamExists(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.teams[id]
	return ok
}

func (s *Store) validateMemberIDsLocked(ids []string) error {
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("%w: empty member id in list", ErrInvalid)
		}
		if _, ok := s.members[id]; !ok {
			return fmt.Errorf("%w: unknown member id %q", ErrInvalid, id)
		}
	}
	return nil
}

// --- copy helpers ----------------------------------------------------------

func cloneMember(m *Member) *Member {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

func cloneTeam(t *Team) *Team {
	if t == nil {
		return nil
	}
	cp := *t
	if t.MemberIDs != nil {
		cp.MemberIDs = append([]string(nil), t.MemberIDs...)
	}
	return &cp
}
