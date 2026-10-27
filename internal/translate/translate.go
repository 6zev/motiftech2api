// Package translate maps between the OpenAI surface and the Motif wire format.
//
// The upstream takes one query string plus server-side conversation state, so the
// gateway's job is: cut the slice of the caller's messages the upstream has not
// seen yet, flatten it, and map only the knobs the upstream actually understands.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/6zev/motiftech2api/internal/store"
	"github.com/6zev/motiftech2api/internal/types"
)

// Model ids the upstream accepts.
const (
	PlainModel     = "motif-12-7b"
	ReasoningModel = "motif-12-7b-reasoning"
)

// QueryBudget leaves headroom under the upstream's 50000-char limit for the role
// markers we add.
const (
	QueryBudget        = 48_000
	UpstreamQueryLimit = 50_000
)

var validModels = map[string]bool{
	PlainModel:     true,
	ReasoningModel: true,
}

// Error is a client-facing gateway error.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// ResolveModel maps a requested id onto the two upstream ids.
func ResolveModel(requested string) (string, error) {
	if requested == "" {
		return PlainModel, nil
	}
	if validModels[requested] {
		return requested, nil
	}
	id := strings.ToLower(requested)
	if strings.Contains(id, "reasoning") ||
		strings.HasPrefix(id, "gpt") || strings.HasPrefix(id, "o1") ||
		strings.HasPrefix(id, "o3") || strings.HasPrefix(id, "o4") {
		return ReasoningModel, nil
	}
	return "", &Error{
		Status:  400,
		Code:    "model_not_found",
		Message: fmt.Sprintf("Unknown model %q. Supported: %s, %s", requested, PlainModel, ReasoningModel),
	}
}

// MapEffort translates OpenAI's value onto the upstream's. The upstream rejects
// "low", so it must be translated and never forwarded; unknown values are dropped.
func MapEffort(requested string) string {
	switch strings.ToLower(requested) {
	case "low", "small":
		return "small"
	case "medium":
		return "medium"
	case "high":
		return "high"
	}
	return ""
}

// hasHangul reports whether s contains a Hangul syllable.
func hasHangul(s string) bool {
	for _, r := range s {
		if r >= 0xAC00 && r <= 0xD7A3 {
			return true
		}
	}
	return false
}

// MessageText extracts the text of a message, handling both a plain string and an
// array of content parts.
func MessageText(m types.Message) string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// DetectLanguage: "ko" when the last user message contains Hangul, else "en".
// The upstream accepts only ko or en.
func DetectLanguage(msgs []types.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if hasHangul(MessageText(msgs[i])) {
			return "ko"
		}
		return "en"
	}
	return "en"
}

// Slice is where to cut the caller's array.
type Slice struct {
	Start          int
	ConversationID *string
}

// ResolveSlice decides the cut using the stored cursor. The store decides
// fresh-versus-chained; messages alone never do, because a caller may rewrite or
// compact its own array between calls.
func ResolveSlice(msgs []types.Message, entry *store.Entry) Slice {
	if entry == nil {
		return Slice{Start: 0, ConversationID: nil}
	}
	start := entry.NextIndex
	if start > len(msgs) {
		start = len(msgs)
	}
	id := entry.ConversationID
	return Slice{Start: start, ConversationID: &id}
}

var roleLabel = map[string]string{
	"system":    "system",
	"developer": "system",
	"user":      "user",
	"assistant": "assistant",
	"tool":      "tool",
}

// FlattenMessages renders messages as one prompt with role markers.
func FlattenMessages(msgs []types.Message) string {
	var blocks []string
	for _, m := range msgs {
		text := strings.TrimSpace(MessageText(m))
		if text == "" {
			continue
		}
		label, ok := roleLabel[m.Role]
		if !ok {
			label = m.Role
		}
		blocks = append(blocks, label+": "+text)
	}
	return strings.Join(blocks, "\n\n")
}

// Built is the result of the cursor step.
type Built struct {
	Query          string
	ConversationID *string
	NextIndex      int
	Language       string
}

// BuildRequest is the single owner of the whole cursor step: cut, flatten, apply
// the empty-slice fallback, and report the cursor the server will commit.
func BuildRequest(msgs []types.Message, entry *store.Entry) (*Built, error) {
	if len(msgs) == 0 {
		return nil, &Error{Status: 400, Code: "invalid_request", Message: "messages must not be empty"}
	}

	cut := ResolveSlice(msgs, entry)
	slice := msgs[cut.Start:]
	query := FlattenMessages(slice)

	if query == "" {
		if cut.Start > 0 {
			// The caller replayed history verbatim and added nothing new: send the
			// last message once rather than an empty query.
			last := msgs[len(msgs)-1:]
			query = FlattenMessages(last)
		}
		if query == "" {
			return nil, &Error{Status: 400, Code: "invalid_request", Message: "messages contain no text content"}
		}
	}

	if n := runeLen(query); n > QueryBudget {
		return nil, &Error{
			Status: 400,
			Code:   "context_length_exceeded",
			Message: fmt.Sprintf("Flattened prompt is %d chars, over the %d budget "+
				"(upstream limit %d). Shorten the history or start a new conversation.",
				n, QueryBudget, UpstreamQueryLimit),
		}
	}

	return &Built{
		Query:          query,
		ConversationID: cut.ConversationID,
		// The upstream appends its own assistant turn, so after this exchange the
		// caller's array grows to len+1. Commit THAT, so a caller replaying
		// [U1, A1, U2] next time starts exactly at U2 instead of resending A1.
		NextIndex: len(msgs) + 1,
		Language:  DetectLanguage(msgs),
	}, nil
}

// runeLen counts characters the way a human-facing limit does.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// FinishReason maps the upstream finish_class onto OpenAI's finish_reason.
func FinishReason(class string) string {
	if class == "incomplete" {
		return "length"
	}
	return "stop"
}

// EstimateUsage approximates tokens from characters (~4 chars per token), because
// the upstream reports no usage.
func EstimateUsage(prompt, completion string) types.Usage {
	p := runeLen(prompt) / 4
	if p < 1 {
		p = 1
	}
	c := runeLen(completion) / 4
	if c < 1 {
		c = 1
	}
	return types.Usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c}
}

// ConversationKey resolves which caller identity owns the conversation.
func ConversationKey(req *types.ChatRequest, headerValue string) string {
	for _, v := range []string{req.ChatID, req.SessionID, req.ConversationID} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if strings.TrimSpace(headerValue) != "" {
		return strings.TrimSpace(headerValue)
	}
	if strings.TrimSpace(req.User) != "" {
		return "user:" + strings.TrimSpace(req.User)
	}
	return "default"
}
