package kubernetes

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// ErrCursorExpired reports a well-formed list cursor that was issued for a
// different list, filter or set of rows than the one the request reads now.
// The client should start again from the first page. It is still an invalid
// argument, so callers that only know ErrInvalidArguments keep answering 400.
var ErrCursorExpired = fmt.Errorf("%w: the list changed since this cursor was issued", ErrInvalidArguments)

// maxSnapshotCursorLength bounds a snapshot cursor before it is decoded.
const maxSnapshotCursorLength = 128

// snapshotCursor binds an offset into an in-memory list to the rows, in their
// exact order, that the reader has already been shown.
//
// Every list served from a snapshot (the committed inventory, or one live read
// sorted in memory) is replaced as the cluster changes. A bare offset applied
// to a newer list repeats or skips rows without telling the reader. A cursor
// for offset n carries a digest of the list's name, the cluster scope, the
// normalized filters and the identities of the first n rows in order. The
// cursor stays valid while that prefix is unchanged, so rows added, removed or
// reordered after the reader's position do not send the reader back to the
// first page; any change before the position does. The digest deliberately
// leaves out observation and refresh timestamps and row fields that do not
// move a row, so routine polling does not expire a cursor.
type snapshotCursor struct {
	header []byte
	rows   []string
}

// bindSnapshotCursor prepares the binding for one list read. rows must hold
// one stable identity per row, in the order the list is served.
func bindSnapshotCursor(list string, scope kubeindex.Scope, filters any, rows []string) (snapshotCursor, error) {
	header, err := json.Marshal(struct {
		List    string
		Scope   kubeindex.Scope
		Filters any
	}{list, scope, filters})
	if err != nil {
		return snapshotCursor{}, err
	}
	return snapshotCursor{header: header, rows: rows}, nil
}

// prefixDigest digests the list header and the identities of the first offset
// rows. Each identity is length-prefixed, so no two different prefixes share
// an encoding.
func (cursor snapshotCursor) prefixDigest(offset int) string {
	hash := sha256.New()
	hash.Write(cursor.header)
	for _, row := range cursor.rows[:offset] {
		hash.Write([]byte("\n" + strconv.Itoa(len(row)) + ":"))
		hash.Write([]byte(row))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// window resolves a client cursor to the page [start, end) of a list of total
// rows and returns the cursor for the following page. A cursor that does not
// decode is an invalid argument. A well-formed cursor whose rows before its
// position, filters, scope or list differ from the current read is expired.
func (cursor snapshotCursor) window(raw string, limit, total int) (int, int, string, error) {
	offset := ""
	if raw != "" {
		if bareSnapshotOffset(raw) {
			// Earlier releases issued bare offsets. They name no rows, so they
			// cannot be served safely; the reader starts the list again.
			return 0, 0, "", ErrCursorExpired
		}
		digest, position, ok := decodeSnapshotCursor(raw)
		if !ok {
			return 0, 0, "", ErrInvalidArguments
		}
		parsed, err := strconv.Atoi(position)
		if err != nil {
			return 0, 0, "", ErrInvalidArguments
		}
		// A position past the end means rows before it were removed.
		if parsed > len(cursor.rows) || parsed > total || cursor.prefixDigest(parsed) != digest {
			return 0, 0, "", ErrCursorExpired
		}
		offset = position
	}
	start, end, next, err := pageWindow(offset, limit, total)
	if err != nil {
		return 0, 0, "", err
	}
	if next == "" {
		return start, end, "", nil
	}
	return start, end, cursor.encode(end), nil
}

// encode returns the cursor that resumes the list at offset.
func (cursor snapshotCursor) encode(offset int) string {
	offset = max(0, min(offset, len(cursor.rows)))
	return base64.RawURLEncoding.EncodeToString([]byte(cursor.prefixDigest(offset) + ":" + strconv.Itoa(offset)))
}

// isSnapshotCursor reports whether raw has the shape of a snapshot list
// cursor, either the bound form or a bare offset from an earlier release. Lists
// that also page saved history use it to route a cursor to the right reader.
func isSnapshotCursor(raw string) bool {
	if bareSnapshotOffset(raw) {
		return true
	}
	_, _, ok := decodeSnapshotCursor(raw)
	return ok
}

func bareSnapshotOffset(raw string) bool {
	parsed, err := strconv.Atoi(raw)
	return err == nil && parsed >= 0 && strconv.Itoa(parsed) == raw
}

func decodeSnapshotCursor(raw string) (string, string, bool) {
	if raw == "" || len(raw) > maxSnapshotCursorLength {
		return "", "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", "", false
	}
	digest, position, ok := strings.Cut(string(decoded), ":")
	if !ok || !snapshotDigestShape(digest) || !bareSnapshotOffset(position) {
		return "", "", false
	}
	return digest, position, true
}

func snapshotDigestShape(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil && strings.ToLower(digest) == digest
}

// snapshotResume lets a caller that shows fewer rows than a page holds resume
// the same list right after the rows it showed.
type snapshotResume struct {
	cursor snapshotCursor
	start  int
	bound  bool
}

func (cursor snapshotCursor) resumeFrom(start int) snapshotResume {
	return snapshotResume{cursor: cursor, start: start, bound: true}
}

func (resume snapshotResume) after(shown int) string {
	if !resume.bound || shown < 0 {
		return ""
	}
	return resume.cursor.encode(resume.start + shown)
}
