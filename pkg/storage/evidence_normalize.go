package storage

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// normalizeEvidenceBatch is the one place evidence text is made storable. Raw
// cluster text can carry NUL, invalid UTF-8 or unpaired UTF-16 surrogates. The
// memory and file stores would keep them, but Postgres cannot hold them in
// text or jsonb, so the same batch would save on one store and stall on
// another. Every store applies this before it validates, hashes or writes, so
// all stores keep and hash identical content.
//
// Payload strings have NUL, invalid UTF-8 and unpaired surrogates replaced by
// U+FFFD. Identity and filter fields have invalid UTF-8 replaced by U+FFFD;
// NUL there stays a validation refusal. A payload that is already storable is
// returned byte for byte, so normal saves and their version hashes are
// unchanged.
func normalizeEvidenceBatch(batch EvidenceBatch) EvidenceBatch {
	if len(batch.Records) > 0 {
		records := make([]EvidenceRecord, len(batch.Records))
		for i, record := range batch.Records {
			record.Payload = normalizeEvidencePayload(record.Payload)
			for _, field := range []*string{&record.ID, &record.Namespace, &record.Name, &record.Kind, &record.AuthorizationKind, &record.UID, &record.RootUID, &record.PodUID, &record.Rule, &record.Epoch} {
				*field = strings.ToValidUTF8(*field, "\uFFFD")
			}
			records[i] = record
		}
		batch.Records = records
	}
	if len(batch.Issues) > 0 {
		issues := make([]EvidenceIssue, len(batch.Issues))
		for i, issue := range batch.Issues {
			issue.Payload = normalizeEvidencePayload(issue.Payload)
			for _, field := range []*string{&issue.Key, &issue.UID, &issue.Kind, &issue.Rule, &issue.Namespace, &issue.AuthorizationKind} {
				*field = strings.ToValidUTF8(*field, "\uFFFD")
			}
			issues[i] = issue
		}
		batch.Issues = issues
	}
	return batch
}

// normalizeEvidencePayload rewrites a JSON payload only when it holds text a
// store cannot keep. Invalid JSON is returned unchanged for validation to
// refuse.
func normalizeEvidencePayload(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 || evidencePayloadStorable(payload) {
		return payload
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.More() {
		return payload
	}
	// Decoding already turned invalid UTF-8 and unpaired surrogates into
	// U+FFFD; NUL decodes to a real NUL and is replaced here.
	value = normalizeEvidenceValue(value)
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return payload
	}
	return json.RawMessage(bytes.TrimRight(out.Bytes(), "\n"))
}

// evidencePayloadStorable reports a payload that needs no rewrite: valid
// UTF-8 without NUL bytes and without any \u0000 or surrogate escape. A
// surrogate escape (U+D800 to U+DFFF) is rewritten even when it is paired,
// which keeps its meaning; every other escape, such as Hangul from U+D000 to
// U+D7FF, leaves the payload byte for byte.
func evidencePayloadStorable(payload []byte) bool {
	if !utf8.Valid(payload) || bytes.IndexByte(payload, 0) >= 0 {
		return false
	}
	for i := 0; i+5 < len(payload); i++ {
		if payload[i] != '\\' {
			continue
		}
		if payload[i+1] != 'u' {
			i++ // skip the escaped character
			continue
		}
		hex := payload[i+2 : i+6]
		if string(hex) == "0000" || evidenceSurrogateEscape(hex) {
			return false
		}
		i += 5
	}
	return true
}

func normalizeEvidenceValue(value any) any {
	switch typed := value.(type) {
	case string:
		return normalizeEvidenceString(typed)
	case []any:
		for i := range typed {
			typed[i] = normalizeEvidenceValue(typed[i])
		}
		return typed
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[normalizeEvidenceString(key)] = normalizeEvidenceValue(item)
		}
		return normalized
	default:
		return value
	}
}

func normalizeEvidenceString(value string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(value, "\uFFFD"), "\x00", "\uFFFD")
}

// evidenceSurrogateEscape reports the four hex digits of a \u escape that name
// a UTF-16 surrogate, U+D800 to U+DFFF.
func evidenceSurrogateEscape(hex []byte) bool {
	if hex[0] != 'd' && hex[0] != 'D' {
		return false
	}
	switch hex[1] {
	case '8', '9', 'a', 'b', 'c', 'd', 'e', 'f', 'A', 'B', 'C', 'D', 'E', 'F':
		return true
	}
	return false
}
