package controllers

import "crypto/subtle"

// secureEqual reports whether a and b are equal in constant time. Used
// by every admin controller's X-Gateway-Secret check so prefix/length
// matches do not leak through response timing. `expected == ""` is
// handled by callers (we never accept an empty configured secret).
func secureEqual(got, expected string) bool {
	if len(got) != len(expected) {
		// subtle.ConstantTimeCompare returns 0 on length mismatch but
		// short-circuits on length anyway — equivalent constant-time
		// behavior from the attacker's perspective is preserved
		// because both branches do the same amount of work for the
		// "wrong length" case.
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}
