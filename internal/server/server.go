// Package server exposes the OpenAI-compatible HTTP surface.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/6zev/motiftech2api/internal/config"
	"github.com/6zev/motiftech2api/internal/store"
	"github.com/6zev/motiftech2api/internal/translate"
	"github.com/6zev/motiftech2api/internal/types"
	"github.com/6zev/motiftech2api/internal/upstream"
)

// Streamer is the upstream call, injectable for tests.
type Streamer interface {
	Stream(ctx context.Context, body types.MotifRequest, fn func(types.Event) error) error
}

// Server wires config, store and upstream together.
type Server struct {
	Cfg      config.Config
	Store    *store.Store
	Upstream Streamer
}

// New builds a server with the real upstream client.
func New(cfg config.Config, st *store.Store) *Server {
	return NewWithClient(cfg, st, nil)
}

// NewWithClient is New with an explicit HTTP client, so the caller can inject a
// proxied transport (http or socks5). A nil client uses a plain one.
func NewWithClient(cfg config.Config, st *store.Store, hc *http.Client) *Server {
	return &Server{
		Cfg:   cfg,
		Store: st,
		Upstream: upstream.NewClient(cfg.UpstreamBase,
			time.Duration(cfg.UpstreamTimeoutMS)*time.Millisecond, hc),
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Cfg.Debug {
		log.Printf(format, args...)
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("access-control-allow-origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	typ := "api_error"
	switch status {
	case 400, 401:
		typ = "invalid_request_error"
	case 404:
		typ = "not_found_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    typ,
			"code":    code,
		},
	})
}

// authorised compares the bearer token against every configured key, with no
// early exit, by hashing both sides to a fixed length.
func (s *Server) authorised(r *http.Request) bool {
	if len(s.Cfg.GatewayAPIKeys) == 0 {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("authorization"))
	const prefix = "bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	given := sha256.Sum256([]byte(strings.TrimSpace(auth[len(prefix):])))

	ok := 0
	for _, k := range s.Cfg.GatewayAPIKeys {
		want := sha256.Sum256([]byte(k))
		ok |= subtle.ConstantTimeCompare(given[:], want[:])
	}
	return ok == 1
}

// ---------- routes ----------

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 404, "not_found", fmt.Sprintf("No route for %s %s", r.Method, r.URL.Path))
	})
	return s.withCORS(mux)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("access-control-allow-origin", "*")
			w.Header().Set("access-control-allow-headers", "authorization,content-type")
			w.Header().Set("access-control-allow-methods", "GET,POST,OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/health" && !s.authorised(r) {
			writeError(w, 401, "invalid_api_key", "Invalid or missing API key.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 404, "not_found", "Use GET /v1/models")
		return
	}
	const created = 1_700_000_000
	writeJSON(w, 200, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": translate.PlainModel, "object": "model", "created": created, "owned_by": "motif"},
			{"id": translate.ReasoningModel, "object": "model", "created": created, "owned_by": "motif"},
		},
	})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 404, "not_found", "Use POST /v1/chat/completions")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, 400, "invalid_json", "Could not read the request body.")
		return
	}
	var req types.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, 400, "invalid_json", "Request body must be valid JSON.")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, 400, "invalid_request", "messages[] is required.")
		return
	}
	if len(req.Tools) > 0 {
		// The upstream is an opaque agent: it never emits tool_calls and cannot
		// accept tool results. Silently ignoring `tools` would look like success
		// while the client waits forever for a tool call.
		writeError(w, 400, "tools_not_supported",
			"This gateway cannot call client tools: the Motif upstream runs its own agent "+
				"and never returns tool_calls. Remove `tools` from the request, or use a "+
				"model provider that supports tool calling for agentic work.")
		return
	}

	model, err := translate.ResolveModel(req.Model)
	if err != nil {
		ge := err.(*translate.Error)
		writeError(w, ge.Status, ge.Code, ge.Message)
		return
	}

	key := translate.ConversationKey(&req, r.Header.Get("x-motif-conversation-id"))
	var entry *store.Entry
	if e, ok := s.Store.Get(key); ok {
		entry = &e
	}

	built, err := translate.BuildRequest(req.Messages, entry)
	if err != nil {
		ge := err.(*translate.Error)
		writeError(w, ge.Status, ge.Code, ge.Message)
		return
	}

	upReq := types.MotifRequest{
		Query:          built.Query,
		ConversationID: built.ConversationID,
		// mode "chat" is required for `model` to be legal upstream.
		Mode:     "chat",
		Model:    model,
		Language: built.Language,
		Timezone: "UTC",
	}
	upReq.ReasoningEffort = translate.MapEffort(req.ReasoningEffort)

	result, err := s.collect(r.Context(), upReq)
	if err != nil {
		s.handleUpstreamError(w, err, key)
		return
	}

	// Cursor commit policy: collect only returns nil when the turn reached the stop
	// sign, so committing here is safe. A mid-stream failure keeps the id we saw but
	// leaves the cursor alone, so a retry resends the turn instead of skipping it.
	if result.conversationID != "" {
		if err := s.Store.Set(key, result.conversationID, built.NextIndex); err != nil {
			s.logf("store write failed: %v", err)
		}
	}

	if req.Stream {
		s.writeStream(w, result, model)
		return
	}
	s.writeCompletion(w, result, model, built.Query)
}

// ---------- upstream result ----------

type chatResult struct {
	content        string
	finishClass    string
	messageID      string
	conversationID string
}

func (s *Server) collect(ctx context.Context, body types.MotifRequest) (*chatResult, error) {
	// The model streams a cycle's text BEFORE the closing chat.step announces what
	// that run was. So each run is buffered and classified when chat.step arrives:
	// narration is discarded, the answer run is appended to the output.
	var answerParts []string
	var currentRun strings.Builder
	var res chatResult

	err := s.Upstream.Stream(ctx, body, func(ev types.Event) error {
		switch ev.Type {
		case types.EvConversationID:
			if id, ok := ev.IDFrom(); ok {
				res.conversationID = id
			}
		case types.EvStep:
			if role, ok := ev.StepRole(); ok && role == "answer" {
				answerParts = append(answerParts, currentRun.String())
			}
			currentRun.Reset()
		case types.EvText:
			if txt, ok := ev.TextOf(); ok {
				currentRun.WriteString(txt)
			}
		case types.EvMessage:
			if id, fc, ok := ev.MessageOf(); ok {
				res.messageID = id
				res.finishClass = fc
			}
		case types.EvError:
			code, msg, ok := ev.ErrorOf()
			if !ok {
				code, msg = "upstream_error", "Upstream error"
			}
			return &translate.Error{Status: 502, Code: code, Message: msg}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	content := strings.Join(answerParts, "")
	if content == "" && currentRun.Len() > 0 {
		// A stream that ended without a closing chat.step still carries usable text.
		content = currentRun.String()
	}
	res.content = content
	return &res, nil
}

func (s *Server) handleUpstreamError(w http.ResponseWriter, err error, key string) {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		if ue.IsConversationNotFound() {
			// Stale id: drop it so the next request starts a fresh conversation.
			if cerr := s.Store.Clear(key); cerr != nil {
				s.logf("store clear failed: %v", cerr)
			}
			writeError(w, 404, "conversation_not_found",
				"The upstream conversation no longer exists; it has been reset. Retry the request.")
			return
		}
		writeError(w, 502, "upstream_error", ue.Detail)
		return
	}
	var ge *translate.Error
	if errors.As(err, &ge) {
		writeError(w, ge.Status, ge.Code, ge.Message)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, 504, "upstream_timeout", "The upstream request timed out.")
		return
	}
	if errors.Is(err, context.Canceled) {
		writeError(w, 499, "client_closed_request", "Client disconnected.")
		return
	}
	s.logf("upstream failure: %v", err)
	writeError(w, 502, "upstream_error", "Upstream request failed.")
}

// ---------- responses ----------

func completionID(seed string) string {
	var b strings.Builder
	for _, r := range seed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 24 {
			break
		}
	}
	if b.Len() == 0 {
		b.WriteString("motif")
	}
	return "chatcmpl-" + b.String()
}

func (s *Server) writeCompletion(w http.ResponseWriter, res *chatResult, model, prompt string) {
	seed := res.messageID
	if seed == "" {
		seed = res.conversationID
	}
	writeJSON(w, 200, map[string]any{
		"id":      completionID(seed),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]string{
				"role":    "assistant",
				"content": res.content,
			},
			"finish_reason": translate.FinishReason(res.finishClass),
		}},
		"usage": translate.EstimateUsage(prompt, res.content),
	})
}

type chunkChoice struct {
	Index        int               `json:"index"`
	Delta        map[string]string `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

func (s *Server) writeStream(w http.ResponseWriter, res *chatResult, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "internal_error", "Streaming unsupported by connection.")
		return
	}

	h := w.Header()
	h.Set("content-type", "text/event-stream; charset=utf-8")
	h.Set("cache-control", "no-cache")
	h.Set("connection", "keep-alive")
	h.Set("access-control-allow-origin", "*")
	w.WriteHeader(200)

	seed := res.messageID
	if seed == "" {
		seed = res.conversationID
	}
	id := completionID(seed)
	created := time.Now().Unix()

	send := func(c chunk) {
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	send(chunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []chunkChoice{{Index: 0, Delta: map[string]string{"role": "assistant", "content": ""}}}})
	if res.content != "" {
		send(chunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []chunkChoice{{Index: 0, Delta: map[string]string{"content": res.content}}}})
	}
	finish := translate.FinishReason(res.finishClass)
	send(chunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []chunkChoice{{Index: 0, Delta: map[string]string{}, FinishReason: &finish}}})

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
