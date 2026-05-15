package teams

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(storage.NewMemory())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestDeriveAlias(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Alice Cooper", "alice-cooper"},
		{"  spaced  out  ", "spaced-out"},
		{"!!Hi--there??", "hi--there"},
		{"Quan Huỳnh", "quan-hu-nh"},
		{"snake_case_ok", "snake_case_ok"},
		{"", ""},
	}
	for _, c := range cases {
		if got := DeriveAlias(c.in); got != c.want {
			t.Errorf("DeriveAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMemberCRUD(t *testing.T) {
	s := newTestStore(t)

	// Create with auto alias + typed meta.
	m, err := s.CreateMember(Member{
		Name: "Alice Cooper",
		Meta: MemberMeta{SlackID: "U001", Email: "alice@example.com"},
	})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}
	if m.Alias != "alice-cooper" {
		t.Errorf("auto alias = %q, want alice-cooper", m.Alias)
	}
	if m.Meta.SlackID != "U001" || m.Meta.Email != "alice@example.com" {
		t.Errorf("Meta not persisted: %+v", m.Meta)
	}

	// Validation.
	if _, err := s.CreateMember(Member{Name: "  "}); err == nil {
		t.Errorf("expected error on empty name")
	}

	// Update — name only, meta untouched.
	got, err := s.UpdateMember(m.ID, Member{Name: "Alice C."}, false)
	if err != nil {
		t.Fatalf("UpdateMember: %v", err)
	}
	if got.Name != "Alice C." {
		t.Errorf("name not updated")
	}
	if got.Meta.SlackID != "U001" {
		t.Errorf("meta clobbered without replaceMeta: %+v", got.Meta)
	}

	// Update with replaceMeta=true clears unset fields.
	got, err = s.UpdateMember(m.ID, Member{Meta: MemberMeta{TelegramID: "9001"}}, true)
	if err != nil {
		t.Fatalf("UpdateMember replace: %v", err)
	}
	if got.Meta.SlackID != "" || got.Meta.TelegramID != "9001" {
		t.Errorf("replace meta failed: %+v", got.Meta)
	}

	// Persistence: reload from same backend.
	s2, err := NewStore(s.provider)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	round, err := s2.GetMember(m.ID)
	if err != nil {
		t.Fatalf("GetMember after reload: %v", err)
	}
	if round.Meta.TelegramID != "9001" {
		t.Errorf("reload lost meta: %+v", round.Meta)
	}

	// Delete.
	if err := s.DeleteMember(m.ID); err != nil {
		t.Fatalf("DeleteMember: %v", err)
	}
	if _, err := s.GetMember(m.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestTeamCRUDAndMemberCleanup(t *testing.T) {
	s := newTestStore(t)
	alice, _ := s.CreateMember(Member{Name: "Alice"})
	bob, _ := s.CreateMember(Member{Name: "Bob"})

	// Unknown member id rejected.
	if _, err := s.CreateTeam(Team{Name: "Eng", MemberIDs: []string{"nope"}}); err == nil {
		t.Errorf("expected ErrInvalid for unknown member id")
	}

	team, err := s.CreateTeam(Team{
		Name:      "Platform Team",
		MemberIDs: []string{alice.ID, bob.ID},
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if team.Alias != "platform-team" {
		t.Errorf("auto alias = %q", team.Alias)
	}

	// Deleting a member scrubs them from teams.
	if err := s.DeleteMember(alice.ID); err != nil {
		t.Fatalf("DeleteMember: %v", err)
	}
	got, err := s.GetTeam(team.ID)
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	if len(got.MemberIDs) != 1 || got.MemberIDs[0] != bob.ID {
		t.Errorf("team member cleanup failed: %v", got.MemberIDs)
	}

	// Update with replaceMembers=true and an empty list clears the list.
	got, err = s.UpdateTeam(team.ID, Team{MemberIDs: nil}, true)
	if err != nil {
		t.Fatalf("UpdateTeam: %v", err)
	}
	if len(got.MemberIDs) != 0 {
		t.Errorf("expected empty members after replace, got %v", got.MemberIDs)
	}

	// TeamExists + ValidateMemberIDs surface helpers.
	if !s.TeamExists(team.ID) {
		t.Errorf("TeamExists false for known team")
	}
	if err := s.ValidateMemberIDs([]string{bob.ID}); err != nil {
		t.Errorf("ValidateMemberIDs: %v", err)
	}
	if err := s.ValidateMemberIDs([]string{"missing"}); err == nil {
		t.Errorf("expected ValidateMemberIDs to reject unknown id")
	}
}
