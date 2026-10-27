package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/6zev/motiftech2api/internal/config"
	"github.com/6zev/motiftech2api/internal/server"
	"github.com/6zev/motiftech2api/internal/store"
)

// liveOnly skips unless E2E=1: these tests write to the real upstream.
func liveOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to run live tests")
	}
}

func startLive(t *testing.T) *httptestServer {
	t.Helper()
	cfg := config.Config{
		UpstreamBase:         "https://chat.motiftech.io",
		UpstreamTimeoutMS:    120_000,
		ConversationTTLHours: 1,
		StorePath:            filepath.Join(t.TempDir(), "conv.json"),
	}
	st := store.New(cfg.StorePath, time.Hour)
	srv := server.New(cfg, st)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return &httptestServer{base: "http://" + ln.Addr().String()}
}

type httptestServer struct{ base string }

func (s *httptestServer) chat(t *testing.T, body any) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res, raw
}

func TestLiveSimplePrompt(t *testing.T) {
	liveOnly(t)
	s := startLive(t)

	res, raw := s.chat(t, map[string]any{
		"model":    "motif-12-7b",
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: PING"}},
	})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["object"] != "chat.completion" || body["model"] != "motif-12-7b" {
		t.Fatalf("body = %v", body)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	content := choice["message"].(map[string]any)["content"].(string)
	if strings.TrimSpace(content) != "PING" {
		t.Fatalf("content = %q", content)
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	usage := body["usage"].(map[string]any)
	if usage["total_tokens"].(float64) <= 0 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestLiveChainedContext(t *testing.T) {
	liveOnly(t)
	s := startLive(t)
	key := fmt.Sprintf("e2e-%d", time.Now().UnixNano())

	_, raw := s.chat(t, map[string]any{
		"chat_id":  key,
		"messages": []map[string]string{{"role": "user", "content": "My name is Zorblax. Just acknowledge."}},
	})
	var first map[string]any
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	firstContent := first["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if strings.TrimSpace(firstContent) == "" {
		t.Fatalf("empty first answer: %s", raw)
	}

	_, raw2 := s.chat(t, map[string]any{
		"chat_id": key,
		"messages": []map[string]string{
			{"role": "user", "content": "My name is Zorblax. Just acknowledge."},
			{"role": "assistant", "content": firstContent},
			{"role": "user", "content": "What is my name?"},
		},
	})
	var second map[string]any
	if err := json.Unmarshal(raw2, &second); err != nil {
		t.Fatal(err)
	}
	secondContent := second["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.Contains(strings.ToLower(secondContent), "zorblax") {
		t.Fatalf("context not carried; answer = %q", secondContent)
	}
}

func TestLiveStream(t *testing.T) {
	liveOnly(t)
	s := startLive(t)

	res, raw := s.chat(t, map[string]any{
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Count from 1 to 3."}},
	})
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, raw)
	}
	if ct := res.Header.Get("content-type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	text := string(raw)
	if n := strings.Count(text, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d", n)
	}
	var content strings.Builder
	for _, f := range strings.Split(text, "\n\n") {
		f = strings.TrimSpace(f)
		if f == "" || f == "data: [DONE]" {
			continue
		}
		obj := strings.TrimPrefix(f, "data: ")
		var c map[string]any
		if err := json.Unmarshal([]byte(obj), &c); err != nil {
			t.Fatalf("bad frame %q: %v", f, err)
		}
		delta := c["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
	}
	if content.Len() == 0 {
		t.Fatal("no streamed content")
	}
}

// Not a live test: the version flag must work without a network or a store.
func TestVersionIsStamped(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "motiftech2api")
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X main.version=v9.9.9-test",
		"-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "v9.9.9-test" {
		t.Fatalf("--version = %q, want v9.9.9-test", got)
	}

	// An unknown argument must fail loudly rather than silently start a server.
	if err := exec.Command(bin, "--nope").Run(); err == nil {
		t.Fatal("unknown argument should exit non-zero")
	}
}
