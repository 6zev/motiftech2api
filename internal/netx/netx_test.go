package netx

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func clearProxyEnv(t *testing.T) {
	t.Helper()
	names := []string{
		"MOTIF_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	}
	for _, n := range names {
		old, had := os.LookupEnv(n)
		os.Unsetenv(n)
		n := n
		t.Cleanup(func() {
			if had {
				os.Setenv(n, old)
			} else {
				os.Unsetenv(n)
			}
		})
	}
}

func TestResolveExplicitWins(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://from-env:1111")
	t.Setenv("MOTIF_PROXY", "socks5://explicit:1080")
	if got := Resolve(); got.URL != "socks5://explicit:1080" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveNoneDisables(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://from-env:1111")
	t.Setenv("MOTIF_PROXY", "none")
	got := Resolve()
	if !got.Disabled || got.URL != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveStandardVars(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("https_proxy", "http://lower:3128")
	if got := Resolve(); got.URL != "http://lower:3128" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveEmpty(t *testing.T) {
	clearProxyEnv(t)
	got := Resolve()
	if got.URL != "" || got.Disabled {
		t.Fatalf("got %+v", got)
	}
}

func TestDescribeRedactsCredentials(t *testing.T) {
	got := Describe(ProxyConfig{URL: "socks5://user:secret@proxy:1080"})
	if strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Fatalf("credentials leaked: %q", got)
	}
	if !strings.Contains(got, "proxy:1080") {
		t.Fatalf("host missing: %q", got)
	}
}

func TestInvalidProxyURL(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("MOTIF_PROXY", "not a url %%")
	if _, err := NewClient(time.Second); err == nil {
		t.Fatal("expected an error for an invalid proxy URL")
	}
}

func TestInvalidSocksURL(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("MOTIF_PROXY", "socks5://")
	if _, err := NewClient(time.Second); err == nil {
		t.Fatal("expected an error for a socks URL without a host")
	}
}

// --- real proxying, over the wire ---

func TestHTTPProxyIsUsed(t *testing.T) {
	clearProxyEnv(t)

	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		w.Write([]byte("from-target"))
	}))
	defer target.Close()

	proxySawRequest := false
	// A plain HTTP forward proxy: for http:// targets the client sends the full
	// URL in the request line, which this handler can see.
	forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxySawRequest = true
		if !r.URL.IsAbs() {
			t.Errorf("expected an absolute URL through the proxy, got %q", r.URL.String())
		}
		res, err := http.Get(r.URL.String())
		if err != nil {
			t.Errorf("proxy fetch: %v", err)
			w.WriteHeader(502)
			return
		}
		defer res.Body.Close()
		io.Copy(w, res.Body)
	}))
	defer forward.Close()

	t.Setenv("MOTIF_PROXY", forward.URL)

	client, err := NewClient(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer res.Body.Close()

	if !proxySawRequest {
		t.Fatal("the proxy was bypassed")
	}
	if !targetHit {
		t.Fatal("the target was never reached")
	}
}

// TestSocksProxyIsUsed runs a minimal SOCKS5 server that records connections and
// forwards to the real target. This proves the socks5:// path dials through it
// rather than silently connecting directly.
func TestSocksProxyIsUsed(t *testing.T) {
	clearProxyEnv(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("through-socks"))
	}))
	defer target.Close()

	var mu sync.Mutex
	conns := 0

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns++
			mu.Unlock()
			go serveSocks5(c)
		}
	}()

	t.Setenv("MOTIF_PROXY", "socks5://"+ln.Addr().String())

	client, err := NewClient(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("request through socks: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if string(body) != "through-socks" {
		t.Fatalf("body = %q", body)
	}

	mu.Lock()
	n := conns
	mu.Unlock()
	if n == 0 {
		t.Fatal("the socks proxy was bypassed")
	}
}

// serveSocks5 implements just enough of RFC 1928 for a CONNECT to an IPv4 or
// domain target: greeting, no-auth, CONNECT, reply, then splice.
func serveSocks5(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))

	buf := make([]byte, 512)

	// greeting: VER NMETHODS METHODS...
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	n := int(buf[1])
	if _, err := io.ReadFull(c, buf[:n]); err != nil {
		return
	}
	c.Write([]byte{0x05, 0x00}) // no auth

	// request: VER CMD RSV ATYP ...
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	if buf[1] != 0x01 { // CONNECT only
		c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	var host string
	switch buf[3] {
	case 0x01: // ipv4
		if _, err := io.ReadFull(c, buf[:4]); err != nil {
			return
		}
		host = net.IP(buf[:4]).String()
	case 0x03: // domain
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return
		}
		l := int(buf[0])
		if _, err := io.ReadFull(c, buf[:l]); err != nil {
			return
		}
		host = string(buf[:l])
	default:
		c.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(buf[:2]))

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 5*time.Second)
	if err != nil {
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()

	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	c.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, c); done <- struct{}{} }()
	go func() { io.Copy(c, upstream); done <- struct{}{} }()
	<-done
}

// A proxy pointing nowhere must surface an error, not fall back to direct.
func TestDeadProxyDoesNotFallBack(t *testing.T) {
	clearProxyEnv(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should-not-reach"))
	}))
	defer target.Close()

	// A port that is closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	t.Setenv("MOTIF_PROXY", "socks5://"+dead)

	client, err := NewClient(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Get(target.URL)
	if err == nil {
		res.Body.Close()
		t.Fatal("expected a connection error, but the request succeeded (was it direct?)")
	}
}

var _ = context.Background
