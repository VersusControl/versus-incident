package settings

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
)

// ExternalIDPrefix marks a Versus-generated external ID.
const ExternalIDPrefix = "vrs-ext-"

// externalIDBytes is the entropy behind the visible ID: 15 bytes encode to
// exactly 24 base32 characters.
const externalIDBytes = 15

// externalIDEncoding is lowercase, unpadded base32 so the ID contains only
// letters and digits and survives copy/paste through CloudFormation and the
// Terraform snippet.
var externalIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrExternalIDUnavailable means the system random source failed. The caller
// must not fall back to a predictable value.
var ErrExternalIDUnavailable = errors.New("external id unavailable")

// GenerateExternalID returns a fresh external ID: the prefix followed by 24
// lowercase base32 characters drawn from crypto/rand.
func GenerateExternalID() (string, error) {
	random := make([]byte, externalIDBytes)
	if _, err := rand.Read(random); err != nil {
		return "", ErrExternalIDUnavailable
	}
	encoded := externalIDEncoding.EncodeToString(random)
	return ExternalIDPrefix + strings.ToLower(encoded), nil
}

// MaskExternalID renders the value shown outside the audited reveal path. The
// prefix and the last four characters stay visible so an operator can tell two
// IDs apart; everything between them is hidden. A value that does not carry the
// prefix keeps only its last four characters.
func MaskExternalID(value string) string {
	if value == "" {
		return ""
	}
	const visibleTail = 4
	prefix := ""
	body := value
	if strings.HasPrefix(value, ExternalIDPrefix) {
		prefix = ExternalIDPrefix
		body = strings.TrimPrefix(value, ExternalIDPrefix)
	}
	if len(body) <= visibleTail {
		return prefix + strings.Repeat("•", len(body))
	}
	return prefix + strings.Repeat("•", len(body)-visibleTail) + body[len(body)-visibleTail:]
}
