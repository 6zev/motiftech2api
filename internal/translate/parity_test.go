package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/6zev/motiftech2api/internal/store"
	"github.com/6zev/motiftech2api/internal/types"
)

func m2(role, text string) types.Message {
	b, _ := json.Marshal(text)
	return types.Message{Role: role, Content: b}
}

// Mirrors every TS translate case, asserted explicitly.
func TestTranslationParity(t *testing.T) {
	// case-insensitive model ids
	for _, in := range []string{"GPT-4O", "O1", "Motif-12-7b-Reasoning"} {
		if _, err := ResolveModel(in); err != nil {
			t.Errorf("ResolveModel(%q) errored: %v", in, err)
		}
	}
	// low -> small, and low never survives
	if got := MapEffort("low"); got != "small" {
		t.Errorf("MapEffort(low)=%q", got)
	}
	if got := MapEffort("LOW"); got != "small" {
		t.Errorf("MapEffort(LOW)=%q", got)
	}
	// Hangul
	if got := DetectLanguage([]types.Message{m2("user", "안녕하세요")}); got != "ko" {
		t.Errorf("hangul=%q", got)
	}
	// only last user message
	if got := DetectLanguage([]types.Message{m2("user", "안녕"), m2("assistant", "hi"), m2("user", "hello")}); got != "en" {
		t.Errorf("lastuser=%q", got)
	}
	// developer -> system
	if got := FlattenMessages([]types.Message{m2("developer", "be terse")}); got != "system: be terse" {
		t.Errorf("dev=%q", got)
	}
	// empty dropped
	if got := FlattenMessages([]types.Message{m2("user", "hi"), m2("user", "   "), m2("assistant", "ok")}); got != "user: hi\n\nassistant: ok" {
		t.Errorf("empty=%q", got)
	}
	// five cursor cases
	mk := func() []types.Message {
		return []types.Message{m2("user", "U1"), m2("assistant", "A1"), m2("user", "U2")}
	}
	e := func(n int) *store.Entry { return &store.Entry{ConversationID: "conv-1", NextIndex: n} }
	if b, _ := BuildRequest(mk(), e(2)); b.Query != "user: U2" {
		t.Errorf("chained=%q", b.Query)
	}
	if b, _ := BuildRequest(mk(), nil); !strings.Contains(b.Query, "U1") || b.ConversationID != nil {
		t.Errorf("fresh=%q", b.Query)
	}
	if b, _ := BuildRequest(mk(), e(3)); b.Query != "user: U2" {
		t.Errorf("replay=%q", b.Query)
	}
	if b, _ := BuildRequest([]types.Message{m2("user", "U1"), m2("user", "U9")}, e(7)); b.Query != "user: U9" {
		t.Errorf("compacted=%q", b.Query)
	}
	if b, _ := BuildRequest([]types.Message{m2("user", "U1"), m2("assistant", "PH"), m2("user", "U2")}, e(1)); b.Query != "assistant: PH\n\nuser: U2" {
		t.Errorf("retry=%q", b.Query)
	}
	// budget edges
	if _, err := BuildRequest([]types.Message{m2("user", strings.Repeat("x", QueryBudget+1))}, nil); err == nil {
		t.Error("over budget accepted")
	}
	if _, err := BuildRequest([]types.Message{m2("user", strings.Repeat("x", QueryBudget-len("user: ")))}, nil); err != nil {
		t.Errorf("at budget rejected: %v", err)
	}
	// conversation keys
	if k := ConversationKey(&types.ChatRequest{ChatID: "c"}, ""); k != "c" {
		t.Errorf("chat_id=%q", k)
	}
	if k := ConversationKey(&types.ChatRequest{SessionID: "s"}, ""); k != "s" {
		t.Errorf("session_id=%q", k)
	}
	if k := ConversationKey(&types.ChatRequest{}, "hdr"); k != "hdr" {
		t.Errorf("header=%q", k)
	}
}
