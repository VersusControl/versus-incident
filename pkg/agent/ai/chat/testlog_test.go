package chat

import (
	"bytes"
	"io"
	"log"
	"sync"
	"testing"
)

var testLogMu sync.Mutex

// Provider unit tests must use fakes or httptest servers, never live endpoints.
func discardExpectedLogs(t *testing.T) {
	t.Helper()
	testLogMu.Lock()
	previous := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() {
		log.SetOutput(previous)
		testLogMu.Unlock()
	})
}

func captureExpectedLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	testLogMu.Lock()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() {
		log.SetOutput(previous)
		testLogMu.Unlock()
	})
	return &output
}
