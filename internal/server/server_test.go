package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/6zev/motiftech2api/internal/config"
	"github.com/6zev/motiftech2api/internal/store"
	"github.com/6zev/motiftech2api/internal/types"
	"github.com/6zev/motiftech2api/internal/upstream"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// stub replays a fixture, optionally failing on the nth call.
type stub struct {
	mu        sync.Mutex
	calls     []types.MotifRequest
	raw       string
	failOn    int
	fail      error
	dropAfter int // emit the first n events then fail; 0 = disabled
}

func (s *stub) Stream(_ context.Context, body types.MotifRequest, fn func(types.Event) error) error {
	s.mu.Lock()
	s.calls = append(s.calls, body)
	n := len(s.calls)
	s.mu.Unlock()

	if s.failOn == n && s.fail != nil {
		return s.fail
	}

	dec := upstream.NewDecoder(strings.NewReader(s.raw))
	emitted := 0
	for {
		ev, err := dec.Next()
		if err != nil {
			return nil
		}
		if err := fn(ev); err != nil {
			return err
		}
		emitted++
		if s.dropAfter > 0 && emitted >= s.dropAfter {
			return errors.New("socket hang up")
		}
	}
}

func (s *stub) Calls() []types.MotifRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.MotifRequest, len(s.calls))
	copy(out, s.calls)
	return out
}

func newServer(t *testing.T, st *stub, keys []string) (*Server, *store.Store) {
	t.Helper()
	cfg := config.Config{
		Port:                 0,
		Host:                 "127.0.0.1",
		UpstreamBase:         "http://stub.test",
		UpstreamTimeoutMS:    5000,
		GatewayAPIKeys:       keys,
		ConversationTTLHours: 1,
		StorePath:            filepath.Join(t.TempDir(), "conv.json"),
	}
	cs := store.New(cfg.StorePath, time.Hour)
	s := &Server{Cfg: cfg, Store: cs, Upstream: st}
	return s, cs
}

func post(t *testing.T, s *Server, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("content-type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func get(s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

// ---------- routes ----------

func TestHealthAndModels(t *testing.T) {
	s, _ := newServer(t, &stub{raw: fixture(t, "sse_clean.txt")}, nil)

	h := get(s, "/health", nil)
	if h.Code != 200 || !strings.Contains(h.Body.String(), `"ok"`) {
		t.Fatalf("health: %d %s", h.Code, h.Body.String())
	}

	m := decode(t, get(s, "/v1/models", nil))
	if m["object"] != "list" {
		t.Fatalf("models object = %v", m["object"])
	}
	data := m["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("want 2 models, got %d", len(data))
	}
	ids := []string{}
	for _, d := range data {
		row := d.(map[string]any)
		ids = append(ids, row["id"].(string))
		if row["object"] != "model" || row["owned_by"] != "motif" {
			t.Fatalf("row = %v", row)
		}
	}
	if ids[0] != "motif-12-7b" || ids[1] != "motif-12-7b-reasoning" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestOptionsPreflight(t *testing.T) {
	s, _ := newServer(t, &stub{}, []string{"k"})
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("access-control-allow-origin") != "*" {
		t.Fatal("missing CORS header")
	}
}

func TestUnknownRoute(t *testing.T) {
	s, _ := newServer(t, &stub{}, nil)
	rec := get(s, "/nope", nil)
	if rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode(t, rec)
	if body["error"].(map[string]any)["code"] != "not_found" {
		t.Fatalf("body = %v", body)
	}
}

// ---------- non-stream ----------

func TestCompletion(t *testing.T) {
	st := &stub{raw: fixture(t, "sse_clean.txt")}
	s, _ := newServer(t, st, nil)

	rec := post(t, s, map[string]any{
		"model":    "motif-12-7b",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	if body["object"] != "chat.completion" {
		t.Fatalf("object = %v", body["object"])
	}
	if body["model"] != "motif-12-7b" {
		t.Fatalf("model = %v", body["model"])
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id = %q", id)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello! How can I help?" {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Fatalf("role = %v", msg["role"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	usage := body["usage"].(map[string]any)
	if usage["total_tokens"].(float64) <= 0 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestUpstreamRequestBody(t *testing.T) {
	st := &stub{raw: fixture(t, "sse_clean.txt")}
	s, _ := newServer(t, st, nil)
	post(t, s, map[string]any{
		"model":    "motif-12-7b",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	calls := st.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if c.Mode != "chat" {
		t.Fatalf("mode = %q", c.Mode)
	}
	if c.Model != "motif-12-7b" {
		t.Fatalf("model = %q", c.Model)
	}
	if c.ConversationID != nil {
		t.Fatalf("conversation_id should be nil on a fresh call, got %v", *c.ConversationID)
	}
}

func TestEffortLowBecomesSmall(t *testing.T) {
	st := &stub{raw: fixture(t, "sse_clean.txt")}
	s, _ := newServer(t, st, nil)
	post(t, s, map[string]any{
		"model":            "motif-12-7b-reasoning",
		"reasoning_effort": "low",
		"messages":         []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	c := st.Calls()[0]
	if c.ReasoningEffort != "small" {
		t.Fatalf("effort = %q, want small", c.ReasoningEffort)
	}
}

func TestUnknownModel(t *testing.T) {
	s, _ := newServer(t, &stub{}, nil)
	rec := post(t, s, map[string]any{
		"model":    "bogus-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	e := decode(t, rec)["error"].(map[string]any)
	if e["code"] != "model_not_found" {
		t.Fatalf("code = %v", e["code"])
	}
	if !strings.Contains(e["message"].(string), "motif-12-7b") {
		t.Fatalf("message = %v", e["message"])
	}
}

func TestToolsRejected(t *testing.T) {
	s, _ := newServer(t, &stub{raw: fixture(t, "sse_clean.txt")}, nil)
	rec := post(t, s, map[string]any{
		"model":    "motif-12-7b",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools":    []map[string]any{{"type": "function"}},
	}, nil)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	e := decode(t, rec)["error"].(map[string]any)
	if e["code"] != "tools_not_supported" {
		t.Fatalf("code = %v", e["code"])
	}
}

func TestMalformedBody(t *testing.T) {
	s, _ := newServer(t, &stub{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMissingMessages(t *testing.T) {
	s, _ := newServer(t, &stub{}, nil)
	rec := post(t, s, map[string]any{"model": "motif-12-7b"}, nil)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestIncompleteMapsToLength(t *testing.T) {
	s, _ := newServer(t, &stub{raw: fixture(t, "sse_incomplete.txt")}, nil)
	rec := post(t, s, map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	choice := decode(t, rec)["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
}

func TestNarrationDiscarded(t *testing.T) {
	s, _ := newServer(t, &stub{raw: fixture(t, "sse_narration.txt")}, nil)
	rec := post(t, s, map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "weather?"}},
	}, nil)
	choice := decode(t, rec)["choices"].([]any)[0].(map[string]any)
	content := choice["message"].(map[string]any)["content"]
	if content != "It is 28C" {
		t.Fatalf("content = %v", content)
	}
}

// ---------- stream ----------

func TestStreamShape(t *testing.T) {
	s, _ := newServer(t, &stub{raw: fixture(t, "sse_clean.txt")}, nil)
	rec := post(t, s, map[string]any{
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, nil)

	if ct := rec.Header().Get("content-type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	frames := []string{}
	for _, f := range strings.Split(rec.Body.String(), "\n\n") {
		if strings.TrimSpace(f) != "" {
			frames = append(frames, strings.TrimPrefix(f, "data: "))
		}
	}
	done := 0
	for _, f := range frames {
		if f == "[DONE]" {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("[DONE] count = %d", done)
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("last frame = %q", frames[len(frames)-1])
	}

	var content strings.Builder
	var firstDelta map[string]any
	var lastFinish any
	for i, f := range frames {
		if f == "[DONE]" {
			break
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(f), &obj); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if obj["object"] != "chat.completion.chunk" {
			t.Fatalf("object = %v", obj["object"])
		}
		choice := obj["choices"].([]any)[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if i == 0 {
			firstDelta = delta
		}
		if c, ok := delta["content"].(string); ok {
			content.WriteString(c)
		}
		lastFinish = choice["finish_reason"]
	}
	if firstDelta["role"] != "assistant" {
		t.Fatalf("first delta role = %v", firstDelta["role"])
	}
	if content.String() != "Hello! How can I help?" {
		t.Fatalf("content = %q", content.String())
	}
	if lastFinish != "stop" {
		t.Fatalf("finish = %v", lastFinish)
	}
}

// ---------- conversation cursor ----------

func TestChainedConversation(t *testing.T) {
	st := &stub{raw: fixture(t, "sse_clean.txt")}
	s, _ := newServer(t, st, nil)

	post(t, s, map[string]any{
		"chat_id":  "sess-1",
		"messages": []map[string]string{{"role": "user", "content": "first"}},
	}, nil)

	post(t, s, map[string]any{
		"chat_id": "sess-1",
		"messages": []map[string]string{
			{"role": "user", "content": "first"},
			{"role": "assistant", "content": "Hello! How can I help?"},
			{"role": "user", "content": "second"},
		},
	}, nil)

	calls := st.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[1].ConversationID == nil || *calls[1].ConversationID != "01M5XR65XYNDJVEB7HA7VH6SEM" {
		t.Fatalf("second conversation_id = %v", calls[1].ConversationID)
	}
	if calls[1].Query != "user: second" {
		t.Fatalf("second query = %q", calls[1].Query)
	}
}

func TestDifferentKeysDoNotShare(t *testing.T) {
	st := &stub{raw: fixture(t, "sse_clean.txt")}
	s, _ := newServer(t, st, nil)
	post(t, s, map[string]any{"chat_id": "a", "messages": []map[string]string{{"role": "user", "content": "x"}}}, nil)
	post(t, s, map[string]any{"chat_id": "b", "messages": []map[string]string{{"role": "user", "content": "y"}}}, nil)

	calls := st.Calls()
	if calls[1].ConversationID != nil {
		t.Fatalf("b should start fresh, got %v", *calls[1].ConversationID)
	}
	if calls[1].Query != "user: y" {
		t.Fatalf("query = %q", calls[1].Query)
	}
}

func TestHeaderConversationKey(t *testing.T) {
	st := &stub{raw: fixture(t, "sse_clean.txt")}
	s, _ := newServer(t, st, nil)
	post(t, s, map[string]any{"messages": []map[string]string{{"role": "user", "content": "x"}}}, nil)
	post(t, s, map[string]any{
		"messages": []map[string]string{
			{"role": "user", "content": "x"},
			{"role": "assistant", "content": "Hello! How can I help?"},
			{"role": "user", "content": "y"},
		},
	}, map[string]string{"x-motif-conversation-id": "default"})

	calls := st.Calls()
	if calls[1].ConversationID == nil {
		t.Fatal("header key should have found the stored conversation")
	}
}

// ---------- cursor commit policy ----------

func TestConversationNotFoundClearsStore(t *testing.T) {
	st := &stub{raw: fixture(t, "sse_clean.txt"), failOn: 2, fail: &upstream.Error{Status: 404, Detail: "conversation not found"}}
	s, cs := newServer(t, st, nil)

	post(t, s, map[string]any{"chat_id": "k", "messages": []map[string]string{{"role": "user", "content": "one"}}}, nil)
	if _, ok := cs.Get("k"); !ok {
		t.Fatal("entry should exist after the first call")
	}

	rec := post(t, s, map[string]any{
		"chat_id": "k",
		"messages": []map[string]string{
			{"role": "user", "content": "one"},
			{"role": "assistant", "content": "Hello! How can I help?"},
			{"role": "user", "content": "two"},
		},
	}, nil)
	if rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	if decode(t, rec)["error"].(map[string]any)["code"] != "conversation_not_found" {
		t.Fatal("wrong error code")
	}
	if _, ok := cs.Get("k"); ok {
		t.Fatal("entry should be cleared")
	}
}

func TestMidStreamFailureKeepsCursor(t *testing.T) {
	// First call completes; second dies after the conversation id.
	st := &stub{raw: fixture(t, "sse_clean.txt")}
	s, cs := newServer(t, st, nil)

	post(t, s, map[string]any{"chat_id": "k", "messages": []map[string]string{{"role": "user", "content": "one"}}}, nil)
	before, ok := cs.Get("k")
	if !ok {
		t.Fatal("entry should exist")
	}

	// Swap in a failing upstream for the second call.
	s.Upstream = &stub{raw: fixture(t, "sse_clean.txt"), dropAfter: 1}
	rec := post(t, s, map[string]any{
		"chat_id": "k",
		"messages": []map[string]string{
			{"role": "user", "content": "one"},
			{"role": "assistant", "content": "Hello! How can I help?"},
			{"role": "user", "content": "two"},
		},
	}, nil)
	if rec.Code != 502 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	after, ok := cs.Get("k")
	if !ok {
		t.Fatal("entry should still exist")
	}
	if after != before {
		t.Fatalf("cursor changed: before=%+v after=%+v", before, after)
	}
}

func TestUpstreamErrorMapsTo502(t *testing.T) {
	st := &stub{raw: "", failOn: 1, fail: &upstream.Error{Status: 500, Detail: "boom"}}
	s, _ := newServer(t, st, nil)
	rec := post(t, s, map[string]any{"messages": []map[string]string{{"role": "user", "content": "x"}}}, nil)
	if rec.Code != 502 {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---------- auth ----------

func TestAuthMissingKey(t *testing.T) {
	s, _ := newServer(t, &stub{}, []string{"secret"})
	rec := get(s, "/v1/models", nil)
	if rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAuthWrongKey(t *testing.T) {
	s, _ := newServer(t, &stub{}, []string{"secret"})
	rec := get(s, "/v1/models", map[string]string{"authorization": "Bearer nope"})
	if rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAuthRightKey(t *testing.T) {
	s, _ := newServer(t, &stub{}, []string{"secret", "other"})
	rec := get(s, "/v1/models", map[string]string{"authorization": "Bearer other"})
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAuthDisabled(t *testing.T) {
	s, _ := newServer(t, &stub{}, nil)
	if rec := get(s, "/v1/models", nil); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHealthNotGated(t *testing.T) {
	s, _ := newServer(t, &stub{}, []string{"secret"})
	if rec := get(s, "/health", nil); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
}
