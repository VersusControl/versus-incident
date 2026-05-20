package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCreateLarkMessage_Firing(t *testing.T) {
	m := CreateLarkMessage("  hello world  \n", false)
	if m.MsgType != "interactive" {
		t.Errorf("MsgType = %q, want interactive", m.MsgType)
	}
	if !strings.HasPrefix(m.Card.Header.Title.Content, "🔴 Firing") {
		t.Errorf("title = %q, want firing prefix", m.Card.Header.Title.Content)
	}
	if m.Card.Header.Title.Tag != "plain_text" {
		t.Errorf("title tag = %q", m.Card.Header.Title.Tag)
	}
	if len(m.Card.Elements) != 1 {
		t.Fatalf("expected 1 element, got %d", len(m.Card.Elements))
	}
	if m.Card.Elements[0].Tag != "markdown" {
		t.Errorf("element tag = %q", m.Card.Elements[0].Tag)
	}
	if m.Card.Elements[0].Content != "hello world" {
		t.Errorf("content not trimmed: %q", m.Card.Elements[0].Content)
	}
}

func TestCreateLarkMessage_Resolved(t *testing.T) {
	m := CreateLarkMessage("ok", true)
	if !strings.HasPrefix(m.Card.Header.Title.Content, "🟢 Resolved") {
		t.Errorf("title = %q, want resolved prefix", m.Card.Header.Title.Content)
	}
}

func TestCreateLarkMessage_JSONShape(t *testing.T) {
	m := CreateLarkMessage("body", false)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"msg_type":"interactive"`, `"tag":"plain_text"`, `"tag":"markdown"`, `"content":"body"`} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %q: %s", want, s)
		}
	}
}
