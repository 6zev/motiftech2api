// Package types holds the upstream wire shapes and the OpenAI surface we expose.
package types

import "encoding/json"

// MotifRequest is the body the upstream accepts. ConversationID is a *string so
// the key is always present: the upstream requires it and rejects its absence,
// while accepting null.
type MotifRequest struct {
	Query             string   `json:"query"`
	ConversationID    *string  `json:"conversation_id"`
	Language          string   `json:"language,omitempty"`
	Mode              string   `json:"mode,omitempty"`
	Model             string   `json:"model,omitempty"`
	ReasoningEffort   string   `json:"reasoning_effort,omitempty"`
	Timezone          string   `json:"timezone,omitempty"`
	FileIDs           []string `json:"file_ids,omitempty"`
	ContinueTruncated *bool    `json:"continue_truncated_answer,omitempty"`
}

// Event is one decoded upstream SSE event. Only the fields we act on are typed;
// the rest are ignored, which keeps unknown event types harmless.
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Event types we distinguish.
const (
	EvText           = "chat.text"
	EvStep           = "chat.step"
	EvCitation       = "chat.citation"
	EvSign           = "shared.sign"
	EvSignTool       = "shared.sign.tool"
	EvStop           = "shared.stop"
	EvError          = "shared.error"
	EvConversationID = "shared.data.conversation_id"
	EvMessage        = "shared.data.message"
	EvDocumentFile   = "shared.data.document_file"
	EvFileID         = "shared.data.file_id"
	EvReset          = "chat.reset"
)

// StopSign terminates a turn. shared.sign.tool uses the same payload but is only
// a per-tool boundary and must NOT end the stream.
const StopSign = "[[HUB_STOP_SIGN]]"

// TextOf extracts the payload of a chat.text event.
func (e Event) TextOf() (string, bool) {
	var s string
	if err := json.Unmarshal(e.Data, &s); err != nil {
		return "", false
	}
	return s, true
}

// StepRole extracts chat.step's role.
func (e Event) StepRole() (string, bool) {
	var v struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return "", false
	}
	return v.Role, true
}

// ErrorOf extracts a shared.error payload.
func (e Event) ErrorOf() (code, message string, ok bool) {
	var v struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return "", "", false
	}
	return v.Code, v.Message, true
}

// MessageOf extracts shared.data.message.
func (e Event) MessageOf() (id, finishClass string, ok bool) {
	var v struct {
		MessageID   string `json:"message_id"`
		FinishClass string `json:"finish_class"`
	}
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return "", "", false
	}
	return v.MessageID, v.FinishClass, true
}

// IDFrom extracts a plain string payload (conversation id, file id).
func (e Event) IDFrom() (string, bool) {
	var s string
	if err := json.Unmarshal(e.Data, &s); err != nil {
		return "", false
	}
	return s, true
}

// ---------- OpenAI ----------

// Message is one element of the caller's messages array.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`
}

// ChatRequest is the OpenAI request we accept. Unknown fields are ignored on
// purpose so clients can send their usual extras.
type ChatRequest struct {
	Model           string            `json:"model"`
	Messages        []Message         `json:"messages"`
	Stream          bool              `json:"stream"`
	ChatID          string            `json:"chat_id"`
	SessionID       string            `json:"session_id"`
	ConversationID  string            `json:"conversation_id"`
	User            string            `json:"user"`
	ReasoningEffort string            `json:"reasoning_effort"`
	Tools           []json.RawMessage `json:"tools"`
}

// Usage mirrors the OpenAI usage object.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
