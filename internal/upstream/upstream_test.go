package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/6zev/motiftech2api/internal/types"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func decodeAll(t *testing.T, raw string) []types.Event {
	t.Helper()
	d := NewDecoder(strings.NewReader(raw))
	var out []types.Event
	for {
		ev, err := d.Next()
		if err != nil {
			break
		}
		out = append(out, ev)
	}
	return out
}

func texts(evs []types.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Type == types.EvText {
			if s, ok := ev.TextOf(); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestDecoderCleanFixture(t *testing.T) {
	got := texts(decodeAll(t, fixture(t, "sse_clean.txt")))
	want := []string{"Hello", "!", " How", " can", " I", " help", "?"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("texts = %q, want %q", got, want)
	}
}

func TestDecoderConversationIDAndSign(t *testing.T) {
	evs := decodeAll(t, fixture(t, "sse_clean.txt"))
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	if evs[0].Type != types.EvConversationID {
		t.Fatalf("first event = %s", evs[0].Type)
	}
	if id, ok := evs[0].IDFrom(); !ok || id != "01M5XR65XYNDJVEB7HA7VH6SEM" {
		t.Fatalf("conversation id = %q", id)
	}
	last := evs[len(evs)-1]
	if last.Type != types.EvSign {
		t.Fatalf("last event = %s, want shared.sign", last.Type)
	}
	if id, _ := last.IDFrom(); id != types.StopSign {
		t.Fatalf("sign = %q", id)
	}
}

// Regression: one JSON event per data line, several lines per frame.
func TestDecoderMultipleDataLinesPerFrame(t *testing.T) {
	raw := ": comment\n" +
		"data: {\"type\":\"chat.text\",\"data\":\"one\"}\n" +
		"data: {\"type\":\"chat.text\",\"data\":\"two\"}\n" +
		"\n"
	got := texts(decodeAll(t, raw))
	if strings.Join(got, ",") != "one,two" {
		t.Fatalf("texts = %q, want one,two", got)
	}
}

// Regression: shared.sign.tool must not end the turn.
func TestDecoderKeepsReadingPastToolSign(t *testing.T) {
	evs := decodeAll(t, fixture(t, "sse_narration.txt"))
	sawTool := false
	for _, ev := range evs {
		if ev.Type == types.EvSignTool {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatal("fixture should contain shared.sign.tool")
	}
	got := strings.Join(texts(evs), "")
	want := "Searching the webIt is 28C"
	if got != want {
		t.Fatalf("texts = %q, want %q", got, want)
	}
}

func TestDecoderByteAtATime(t *testing.T) {
	raw := fixture(t, "sse_clean.txt")
	pr, pw := newSlowReader(raw)
	go func() { _, _ = pw.Write([]byte(raw)); _ = pw.Close() }()

	d := NewDecoder(pr)
	var all []string
	for {
		ev, err := d.Next()
		if err != nil {
			break
		}
		if ev.Type == types.EvText {
			s, _ := ev.TextOf()
			all = append(all, s)
		}
	}
	if got := strings.Join(all, ""); got != "Hello! How can I help?" {
		t.Fatalf("got %q", got)
	}
}

func TestDecoderIgnoresMalformed(t *testing.T) {
	got := decodeAll(t, "data: not json\n\n")
	if len(got) != 0 {
		t.Fatalf("expected no events, got %d", len(got))
	}
}

func TestDecoderErrorFixture(t *testing.T) {
	evs := decodeAll(t, fixture(t, "sse_error.txt"))
	if len(evs) != 1 || evs[0].Type != types.EvError {
		t.Fatalf("events = %+v", evs)
	}
	code, msg, ok := evs[0].ErrorOf()
	if !ok || code != "upstream_unavailable" {
		t.Fatalf("code = %q msg = %q", code, msg)
	}
}

func TestDecoderIncompleteFixture(t *testing.T) {
	evs := decodeAll(t, fixture(t, "sse_incomplete.txt"))
	var finish string
	for _, ev := range evs {
		if ev.Type == types.EvMessage {
			_, fc, _ := ev.MessageOf()
			finish = fc
		}
	}
	if finish != "incomplete" {
		t.Fatalf("finish_class = %q", finish)
	}
}

// --- Client ---

func newStub(t *testing.T, status int, body string, wantOrigin string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantOrigin != "" && r.Header.Get("origin") != wantOrigin {
			t.Errorf("origin = %q, want %q", r.Header.Get("origin"), wantOrigin)
		}
		if r.Header.Get("accept") != "text/event-stream" {
			t.Errorf("accept = %q", r.Header.Get("accept"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestClientStopsAtStopSign(t *testing.T) {
	raw := fixture(t, "sse_clean.txt") + "data: {\"type\":\"chat.text\",\"data\":\"AFTER\"}\n\n"
	srv := newStub(t, 200, raw, "")
	defer srv.Close()

	c := NewClient(srv.URL, 5e9, nil)
	var got []string
	err := c.Stream(context.Background(), types.MotifRequest{Query: "hi"}, func(ev types.Event) error {
		if ev.Type == types.EvText {
			s, _ := ev.TextOf()
			got = append(got, s)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != "Hello! How can I help?" {
		t.Fatalf("got %q", strings.Join(got, ""))
	}
}

func TestClientStopsAfterError(t *testing.T) {
	raw := "data: {\"type\":\"chat.text\",\"data\":\"partial\"}\n\n" +
		fixture(t, "sse_error.txt") +
		"data: {\"type\":\"chat.text\",\"data\":\"AFTER\"}\n\n"
	srv := newStub(t, 200, raw, "")
	defer srv.Close()

	c := NewClient(srv.URL, 5e9, nil)
	var got []string
	_ = c.Stream(context.Background(), types.MotifRequest{Query: "hi"}, func(ev types.Event) error {
		if ev.Type == types.EvText {
			s, _ := ev.TextOf()
			got = append(got, s)
		}
		return nil
	})
	if strings.Join(got, "") != "partial" {
		t.Fatalf("got %q", strings.Join(got, ""))
	}
}

func TestClientSendsOriginAndErrors(t *testing.T) {
	srv := newStub(t, 400, `{"detail":"Invalid request body from client"}`, "")
	defer srv.Close()

	c := NewClient(srv.URL, 5e9, nil)
	err := c.Stream(context.Background(), types.MotifRequest{Query: "hi"}, func(types.Event) error { return nil })
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if ue.Status != 400 || ue.Detail != "Invalid request body from client" {
		t.Fatalf("status=%d detail=%q", ue.Status, ue.Detail)
	}
}

func TestClientConversationNotFound(t *testing.T) {
	srv := newStub(t, 404, `{"detail":"conversation not found"}`, "")
	defer srv.Close()

	c := NewClient(srv.URL, 5e9, nil)
	err := c.Stream(context.Background(), types.MotifRequest{Query: "hi"}, func(types.Event) error { return nil })
	var ue *Error
	if !errors.As(err, &ue) || !ue.IsConversationNotFound() {
		t.Fatalf("want conversation-not-found, got %v", err)
	}
}

// The origin header must be the upstream base, never the gateway host.
func TestClientPinsOriginToUpstream(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("origin")
		_, _ = w.Write([]byte(fixture(t, "sse_clean.txt")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 5e9, nil)
	_ = c.Stream(context.Background(), types.MotifRequest{Query: "hi"}, func(types.Event) error { return nil })
	if seen != srv.URL {
		t.Fatalf("origin = %q, want %q", seen, srv.URL)
	}
}
