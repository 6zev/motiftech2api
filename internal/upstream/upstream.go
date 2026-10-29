// Package upstream talks to the Motif chat service and decodes its SSE stream.
//
// Two behaviours here are load-bearing and were each a bug once:
//
//  1. The upstream puts exactly ONE complete JSON event per "data:" line. Joining
//     the data lines of a frame with newlines (what the SSE spec suggests for
//     multi-line payloads) produces invalid JSON, because these payloads are not
//     split across lines. Several data lines in one frame therefore mean several
//     events, not one concatenated payload.
//  2. "shared.sign.tool" carries the same [[HUB_STOP_SIGN]] payload as the final
//     "shared.sign", but it is only a per-tool boundary. Ending the stream on it
//     truncates every turn that used a tool.
package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/6zev/motiftech2api/internal/types"
)

// Error is a non-2xx upstream response.
type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("upstream %d: %s", e.Status, e.Detail) }

// IsConversationNotFound reports the stale-conversation case, which is a client
// error (the id is gone) rather than an upstream failure.
func (e *Error) IsConversationNotFound() bool {
	return e.Status == 404 && strings.Contains(strings.ToLower(e.Detail), "conversation not found")
}

// Decoder turns a byte stream into parsed events.
type Decoder struct {
	br    *bufio.Reader
	queue []types.Event // events parsed from the current frame, not yet returned
}

// NewDecoder wraps r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{br: bufio.NewReaderSize(r, 32*1024)}
}

// Next returns the next complete event, or io.EOF at the end of the body.
//
// Framing: events are separated by a blank line. Within a frame every "data:"
// line is a complete payload of its own. "event:", "id:", "retry:" and
// ":"-comments are ignored.
func (d *Decoder) Next() (types.Event, error) {
	for {
		if len(d.queue) > 0 {
			ev := d.queue[0]
			d.queue = d.queue[1:]
			return ev, nil
		}

		line, readErr := d.br.ReadString('\n')
		if readErr != nil && line == "" {
			return types.Event{}, readErr // io.EOF or a real error
		}

		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")

		switch {
		case line == "":
			// Frame boundary. If the frame produced nothing and we are at EOF,
			// surface the EOF rather than looping forever.
			if readErr != nil {
				return types.Event{}, readErr
			}
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			var ev types.Event
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue // a malformed line is skipped, not fatal
			}
			d.queue = append(d.queue, ev)
		default:
			// event:/id:/retry:/:comment — ignored by design
		}

		if readErr != nil && line != "" {
			// Trailing line without a final newline: flush what we have.
			if len(d.queue) == 0 {
				return types.Event{}, readErr
			}
		}
	}
}

// Client posts chat requests to the upstream.
type Client struct {
	Base    string
	Timeout time.Duration
	HTTP    *http.Client
}

// NewClient builds a client with the given transport. A dedicated http.Client is
// used so the timeout is per-gateway rather than global; the caller supplies the
// transport so outbound proxy settings (http or socks5) apply here too.
func NewClient(base string, timeout time.Duration, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{Base: base, Timeout: timeout, HTTP: hc}
}

// NewClientWithTransport is NewClient for callers that already resolved a
// transport (see internal/netx for proxy handling).
func NewClientWithTransport(base string, timeout time.Duration, rt http.RoundTripper) *Client {
	return &Client{
		Base:    base,
		Timeout: timeout,
		HTTP:    &http.Client{Transport: rt, Timeout: timeout},
	}
}

// Stream issues the request and hands each event to fn until the turn ends.
// fn returning an error aborts the stream.
func (c *Client) Stream(ctx context.Context, body types.MotifRequest, fn func(types.Event) error) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Base+"/api/v1/enterprise/chat", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("user-agent", "motiftech2api/1.0")
	// Pinned deliberately: the upstream's CORS config allows only its own origin,
	// so this must never be the gateway's host.
	req.Header.Set("origin", c.Base)

	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		detail := strings.TrimSpace(string(raw))
		var env struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Detail != "" {
			detail = env.Detail
		}
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return &Error{Status: res.StatusCode, Detail: detail}
	}

	dec := NewDecoder(res.Body)
	for {
		ev, err := dec.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		if err := fn(ev); err != nil {
			return err
		}

		switch ev.Type {
		case types.EvSign:
			if id, ok := ev.IDFrom(); ok && id == types.StopSign {
				return nil
			}
		case types.EvError:
			return nil // delivered to the caller via fn
		}
	}
}
