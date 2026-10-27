package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/6zev/motiftech2api/internal/store"
	"github.com/6zev/motiftech2api/internal/types"
)

func msg(role, text string) types.Message {
	b, _ := json.Marshal(text)
	return types.Message{Role: role, Content: b}
}

func entry(nextIndex int) *store.Entry {
	return &store.Entry{ConversationID: "conv-1", NextIndex: nextIndex}
}

func TestResolveModel(t *testing.T) {
	cases := map[string]string{
		PlainModel:     PlainModel,
		ReasoningModel: ReasoningModel,
		"":             PlainModel,
		"gpt-4o":       ReasoningModel,
		"gpt-4.1-mini": ReasoningModel,
		"o1":           ReasoningModel,
		"o3-mini":      ReasoningModel,
		"o4-mini":      ReasoningModel,
	}
	for in, want := range cases {
		got, err := ResolveModel(in)
		if err != nil || got != want {
			t.Errorf("ResolveModel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestResolveModelRejectsUnknown(t *testing.T) {
	_, err := ResolveModel("bogus-model")
	ge, ok := err.(*Error)
	if !ok || ge.Status != 400 || ge.Code != "model_not_found" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ge.Message, PlainModel) {
		t.Fatalf("message should list supported ids: %q", ge.Message)
	}
}

func TestMapEffortNeverForwardsLow(t *testing.T) {
	cases := map[string]string{
		"low":    "small",
		"small":  "small",
		"medium": "medium",
		"high":   "high",
		"":       "",
		"banana": "",
	}
	for in, want := range cases {
		if got := MapEffort(in); got != want {
			t.Errorf("MapEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectLanguage(t *testing.T) {
	if got := DetectLanguage([]types.Message{msg("user", "안녕하세요")}); got != "ko" {
		t.Errorf("got %q", got)
	}
	if got := DetectLanguage([]types.Message{msg("user", "hello")}); got != "en" {
		t.Errorf("got %q", got)
	}
	// Only the LAST user message counts.
	if got := DetectLanguage([]types.Message{msg("user", "안녕"), msg("assistant", "hi"), msg("user", "hello")}); got != "en" {
		t.Errorf("got %q", got)
	}
	if got := DetectLanguage([]types.Message{msg("system", "be nice")}); got != "en" {
		t.Errorf("got %q", got)
	}
	// Content parts.
	parts := types.Message{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"반가워"}]`)}
	if got := DetectLanguage([]types.Message{parts}); got != "ko" {
		t.Errorf("got %q", got)
	}
}

func TestFlattenMessages(t *testing.T) {
	got := FlattenMessages([]types.Message{msg("user", "hi"), msg("assistant", "hello"), msg("user", "bye")})
	want := "user: hi\n\nassistant: hello\n\nuser: bye"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFlattenDropsEmpty(t *testing.T) {
	got := FlattenMessages([]types.Message{msg("user", "hi"), msg("user", "   "), msg("assistant", "ok")})
	if got != "user: hi\n\nassistant: ok" {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenDeveloperAsSystem(t *testing.T) {
	if got := FlattenMessages([]types.Message{msg("developer", "be terse")}); got != "system: be terse" {
		t.Fatalf("got %q", got)
	}
}

// The five named cursor cases.

func TestCaseChained(t *testing.T) {
	msgs := []types.Message{msg("user", "U1"), msg("assistant", "A1"), msg("user", "U2")}
	s := ResolveSlice(msgs, entry(2))
	if s.Start != 2 || s.ConversationID == nil || *s.ConversationID != "conv-1" {
		t.Fatalf("slice = %+v", s)
	}
	b, err := BuildRequest(msgs, entry(2))
	if err != nil {
		t.Fatal(err)
	}
	if b.Query != "user: U2" {
		t.Fatalf("query = %q", b.Query)
	}
}

func TestCaseFresh(t *testing.T) {
	msgs := []types.Message{msg("user", "U1"), msg("assistant", "A1"), msg("user", "U2")}
	s := ResolveSlice(msgs, nil)
	if s.Start != 0 || s.ConversationID != nil {
		t.Fatalf("slice = %+v", s)
	}
	b, _ := BuildRequest(msgs, nil)
	if b.Query != "user: U1\n\nassistant: A1\n\nuser: U2" {
		t.Fatalf("query = %q", b.Query)
	}
}

func TestCaseReplay(t *testing.T) {
	msgs := []types.Message{msg("user", "U1"), msg("assistant", "A1"), msg("user", "U2")}
	b, err := BuildRequest(msgs, entry(3))
	if err != nil {
		t.Fatal(err)
	}
	if b.Query != "user: U2" {
		t.Fatalf("query = %q", b.Query)
	}
}

func TestCaseCompacted(t *testing.T) {
	msgs := []types.Message{msg("user", "U1"), msg("user", "U9")}
	if s := ResolveSlice(msgs, entry(7)); s.Start != 2 {
		t.Fatalf("start = %d", s.Start)
	}
	b, _ := BuildRequest(msgs, entry(7))
	if b.Query != "user: U9" {
		t.Fatalf("query = %q", b.Query)
	}
}

// Cursor 1 after a first call that sent only [U1] and then died.
func TestCaseRetryMidstream(t *testing.T) {
	msgs := []types.Message{msg("user", "U1"), msg("assistant", "A1-placeholder"), msg("user", "U2")}
	if s := ResolveSlice(msgs, entry(1)); s.Start != 1 {
		t.Fatalf("start = %d", s.Start)
	}
	b, _ := BuildRequest(msgs, entry(1))
	if b.Query != "assistant: A1-placeholder\n\nuser: U2" {
		t.Fatalf("query = %q", b.Query)
	}
}

// The upstream appends its own reply, so the cursor must be len+1.
func TestNextIndexIncludesUpstreamReply(t *testing.T) {
	b, _ := BuildRequest([]types.Message{msg("user", "x")}, nil)
	if b.NextIndex != 2 {
		t.Fatalf("nextIndex = %d, want 2", b.NextIndex)
	}
	b, _ = BuildRequest([]types.Message{msg("user", "x"), msg("assistant", "y")}, nil)
	if b.NextIndex != 3 {
		t.Fatalf("nextIndex = %d, want 3", b.NextIndex)
	}
}

func TestRejectsEmpty(t *testing.T) {
	if _, err := BuildRequest(nil, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestRejectsOverBudget(t *testing.T) {
	_, err := BuildRequest([]types.Message{msg("user", strings.Repeat("x", QueryBudget+10))}, nil)
	ge, ok := err.(*Error)
	if !ok || ge.Code != "context_length_exceeded" || ge.Status != 400 {
		t.Fatalf("err = %v", err)
	}
}

func TestAcceptsExactlyAtBudget(t *testing.T) {
	// "user: " is 6 chars of marker; keep the whole flattened query at the budget.
	text := strings.Repeat("x", QueryBudget-len("user: "))
	if _, err := BuildRequest([]types.Message{msg("user", text)}, nil); err != nil {
		t.Fatalf("should accept: %v", err)
	}
}

func TestFinishReason(t *testing.T) {
	cases := map[string]string{
		"clean":      "stop",
		"incomplete": "length",
		"repetition": "stop",
		"unknown":    "stop",
		"":           "stop",
	}
	for in, want := range cases {
		if got := FinishReason(in); got != want {
			t.Errorf("FinishReason(%q) = %q want %q", in, got, want)
		}
	}
}

func TestConversationKey(t *testing.T) {
	if got := ConversationKey(&types.ChatRequest{ChatID: "c1"}, "h1"); got != "c1" {
		t.Errorf("got %q", got)
	}
	if got := ConversationKey(&types.ChatRequest{SessionID: "s1"}, ""); got != "s1" {
		t.Errorf("got %q", got)
	}
	if got := ConversationKey(&types.ChatRequest{}, "h1"); got != "h1" {
		t.Errorf("got %q", got)
	}
	if got := ConversationKey(&types.ChatRequest{User: "bob"}, ""); got != "user:bob" {
		t.Errorf("got %q", got)
	}
	if got := ConversationKey(&types.ChatRequest{}, ""); got != "default" {
		t.Errorf("got %q", got)
	}
}
