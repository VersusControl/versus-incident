package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

const distributedRateCASAttempts = 64

// RateLimiter caps the number of AI calls per rolling hour. Pure
// in-memory: a process restart resets the window, which is acceptable
// because the goal is to bound cost per running process — not to enforce
// a quota across a fleet (we run one agent worker per replica).
//
// max <= 0 disables the limit (Allow always returns true).
type RateLimiter struct {
	max int

	mu        sync.Mutex
	bucket    string // current "yyyymmddhh" key
	hourCount int
	keyCounts map[string]int
	provider  storage.Provider
	scope     string
	now       func() time.Time
	cleaned   string
}

type distributedRateBucket struct {
	Count     int            `json:"count"`
	KeyCounts map[string]int `json:"key_counts,omitempty"`
}

// NewRateLimiter constructs a limiter with the configured per-hour cap.
func NewRateLimiter(maxPerHour int) *RateLimiter {
	return &RateLimiter{max: maxPerHour, keyCounts: make(map[string]int), now: time.Now}
}

// NewDistributedRateLimiter constructs an hourly limiter whose aggregate and
// per-key counts are shared through BlobCAS. Providers without BlobCAS retain
// the single-process limiter behavior.
func NewDistributedRateLimiter(maxPerHour int, provider storage.Provider, scope string, now func() time.Time) *RateLimiter {
	limiter := NewRateLimiter(maxPerHour)
	if now != nil {
		limiter.now = now
	}
	if _, ok := provider.(storage.BlobCAS); ok {
		hash := sha256.Sum256([]byte(scope))
		limiter.provider = provider
		limiter.scope = hex.EncodeToString(hash[:8])
	}
	return limiter
}

// Allow returns true and records a use when the caller is under the
// per-hour cap, false otherwise. Thread-safe.
func (r *RateLimiter) Allow() bool {
	return r.AllowKey("")
}

// AllowKey checks both the process-wide hourly ceiling and a per-key fairness
// share. Empty key preserves the original global-only behavior.
func (r *RateLimiter) AllowKey(key string) bool {
	if r == nil || r.max <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := bucketKey(r.now())
	if now != r.bucket {
		r.bucket = now
		r.hourCount = 0
		r.keyCounts = make(map[string]int)
	}
	if r.provider != nil {
		allowed := r.allowDistributed(now, key)
		if allowed {
			r.hourCount++
			if key != "" {
				r.keyCounts[key]++
			}
		}
		return allowed
	}
	if r.hourCount >= r.max {
		return false
	}
	if key != "" {
		keyMax := (r.max + 1) / 2
		if r.keyCounts[key] >= keyMax {
			return false
		}
		r.keyCounts[key]++
	}
	r.hourCount++
	return true
}

func (r *RateLimiter) allowDistributed(bucket, key string) bool {
	cas := r.provider.(storage.BlobCAS)
	name := "ai-rate/" + r.scope + "/" + bucket
	keyHash := ""
	if key != "" {
		hash := sha256.Sum256([]byte(key))
		keyHash = hex.EncodeToString(hash[:8])
	}
	for attempt := 0; attempt < distributedRateCASAttempts; attempt++ {
		current, err := r.provider.ReadBlob(name)
		if err != nil {
			return false
		}
		state := distributedRateBucket{KeyCounts: make(map[string]int)}
		if len(current) > 0 && json.Unmarshal(current, &state) != nil {
			return false
		}
		if state.Count >= r.max {
			return false
		}
		if keyHash != "" && state.KeyCounts[keyHash] >= (r.max+1)/2 {
			return false
		}
		state.Count++
		if keyHash != "" {
			state.KeyCounts[keyHash]++
		}
		replacement, err := json.Marshal(state)
		if err != nil {
			return false
		}
		swapped, err := cas.CompareAndSwapBlob(name, current, replacement)
		if err != nil {
			return false
		}
		if swapped {
			if r.cleaned != bucket {
				if r.cleanupDistributedBuckets(name, bucket) {
					r.cleaned = bucket
				}
			}
			return true
		}
	}
	return false
}

func (r *RateLimiter) cleanupDistributedBuckets(currentName, currentBucket string) bool {
	prefix := "ai-rate/" + r.scope + "/"
	blobs, err := r.provider.ListBlobs(prefix)
	if err != nil {
		return false
	}
	cas := r.provider.(storage.BlobCAS)
	complete := true
	for _, blob := range blobs {
		bucket := blob.Name[len(prefix):]
		if blob.Name != currentName && bucket < currentBucket {
			if swapped, cleanupErr := cas.CompareAndSwapBlob(blob.Name, blob.Data, nil); cleanupErr != nil || !swapped {
				complete = false
			}
		}
	}
	return complete
}

// Stats returns the current hour bucket and how many calls have been
// allowed in it. Used by the admin status endpoint.
func (r *RateLimiter) Stats() (bucket string, used, max int) {
	if r == nil {
		return "", 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bucket, r.hourCount, r.max
}

func bucketKey(t time.Time) string {
	return t.UTC().Format("2006010215")
}
