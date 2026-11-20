package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// DefaultPreferredServices is the Changes page's service filter for a user
// who never chose one: the services whose changes most often matter.
var DefaultPreferredServices = []string{"rds", "iam"}

// changePreferencesPrefix names the blobs that hold each user's Changes
// page preferences. The deployment has one org, so the user alone keys it.
const changePreferencesPrefix = "settings/aws-changes-preferences/"

// ErrNoUser is a preference read or write without a signed-in user.
var ErrNoUser = errors.New("aws changes: no signed-in user")

// PreferenceBlobs is the small-settings store the preferences live in.
type PreferenceBlobs interface {
	ReadBlob(name string) ([]byte, error)
	WriteBlob(name string, data []byte) error
}

// ChangePreferences is one user's Changes page choices.
type ChangePreferences struct {
	// Services is the service filter; empty means every service. Names
	// with no stored changes are kept.
	Services []string `json:"services"`
}

// ChangePreferenceStore keeps each signed-in user's Changes page choices.
type ChangePreferenceStore struct {
	blobs PreferenceBlobs
}

// NewChangePreferenceStore keeps preferences in blobs. A nil store answers
// the defaults and refuses to save.
func NewChangePreferenceStore(blobs PreferenceBlobs) *ChangePreferenceStore {
	return &ChangePreferenceStore{blobs: blobs}
}

func changePreferencesKey(user string) string {
	sum := sha256.Sum256([]byte(user))
	return changePreferencesPrefix + hex.EncodeToString(sum[:16])
}

func defaultChangePreferences() ChangePreferences {
	return ChangePreferences{Services: append([]string(nil), DefaultPreferredServices...)}
}

// Get returns the user's saved choices, or the defaults when none are
// saved. A stored record that no longer reads is the defaults too.
func (store *ChangePreferenceStore) Get(user string) (ChangePreferences, error) {
	if user == "" {
		return ChangePreferences{}, ErrNoUser
	}
	if store == nil || store.blobs == nil {
		return defaultChangePreferences(), nil
	}
	data, err := store.blobs.ReadBlob(changePreferencesKey(user))
	if err != nil {
		return ChangePreferences{}, fmt.Errorf("aws changes: read preferences: %w", err)
	}
	if len(data) == 0 {
		return defaultChangePreferences(), nil
	}
	var stored ChangePreferences
	if json.Unmarshal(data, &stored) != nil {
		return defaultChangePreferences(), nil
	}
	services, err := NormalizeServices(stored.Services)
	if err != nil {
		return defaultChangePreferences(), nil
	}
	return ChangePreferences{Services: services}, nil
}

// Save stores the user's choices and returns them as stored. A malformed
// service name is ErrBadFeedQuery.
func (store *ChangePreferenceStore) Save(user string, preferences ChangePreferences) (ChangePreferences, error) {
	if user == "" {
		return ChangePreferences{}, ErrNoUser
	}
	if store == nil || store.blobs == nil {
		return ChangePreferences{}, ErrNoStorage
	}
	services, err := NormalizeServices(preferences.Services)
	if err != nil {
		return ChangePreferences{}, err
	}
	saved := ChangePreferences{Services: services}
	data, err := json.Marshal(saved)
	if err != nil {
		return ChangePreferences{}, err
	}
	if err := store.blobs.WriteBlob(changePreferencesKey(user), data); err != nil {
		return ChangePreferences{}, fmt.Errorf("aws changes: save preferences: %w", err)
	}
	return saved, nil
}
