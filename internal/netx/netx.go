// Package netx builds the HTTP transport used for outbound requests (the Motif
// upstream and the self-updater).
//
// Proxy resolution order, highest first:
//  1. MOTIF_PROXY explicitly set    (supports http://, https://, socks5://, socks5h://)
//  2. the standard HTTPS_PROXY / HTTP_PROXY / NO_PROXY variables
//  3. no proxy
//
// Setting NO_PROXY=* (or MOTIF_PROXY=none) disables proxying entirely, which is
// what you want inside a docker network where the default env would break it.
package netx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ProxyConfig is the resolved outbound proxy setting.
type ProxyConfig struct {
	// URL is empty when no proxy is configured.
	URL string
	// Disabled is true when the operator explicitly turned proxying off.
	Disabled bool
}

// Resolve says which proxy outbound requests should use.
func Resolve() ProxyConfig {
	explicit := strings.TrimSpace(os.Getenv("MOTIF_PROXY"))
	switch strings.ToLower(explicit) {
	case "none", "off", "direct", "-":
		return ProxyConfig{Disabled: true}
	}
	if explicit != "" {
		return ProxyConfig{URL: explicit}
	}
	// Standard variables. Go's http.ProxyFromEnvironment reads these, but we
	// resolve them here so a socks:// URL can be detected and handled.
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return ProxyConfig{URL: v}
		}
	}
	if strings.TrimSpace(os.Getenv("NO_PROXY")) == "*" {
		return ProxyConfig{Disabled: true}
	}
	return ProxyConfig{}
}

// NewClient returns an *http.Client honouring the resolved proxy setting.
//
// A socks5/https proxy is wired through a custom DialContext because
// http.ProxyURL alone does not speak SOCKS. Plain http proxies use the stdlib
// path, which also keeps CONNECT behaviour correct for https targets.
func NewClient(timeout time.Duration) (*http.Client, error) {
	pc := Resolve()

	transport := &http.Transport{
		// Sensible defaults for a long-lived streaming client.
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Do not buffer: the upstream streams for minutes.
		ResponseHeaderTimeout: 90 * time.Second,
	}

	switch {
	case pc.Disabled || pc.URL == "":
		// No proxy. Explicitly nil so a stray ProxyFromEnvironment cannot leak in.
		transport.Proxy = nil

	case strings.HasPrefix(strings.ToLower(pc.URL), "socks"):
		dialer, err := socksDialer(pc.URL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}

	default:
		u, err := url.Parse(pc.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", pc.URL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid proxy URL %q: expected scheme://host:port", pc.URL)
		}
		transport.Proxy = http.ProxyURL(u)
	}

	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// socksDialer builds a SOCKS5 dialer, with optional user:pass auth encoded in the URL.
func socksDialer(raw string) (proxy.Dialer, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid socks proxy URL %q: %w", raw, err)
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" && u.Scheme != "socks" {
		return nil, fmt.Errorf("invalid socks proxy URL %q: want socks5://", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid socks proxy URL %q: missing host:port", raw)
	}

	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}

	// For socks5h the name resolution happens at the proxy, which is what
	// golang.org/x/net/proxy does by default for dialing.
	d, err := proxy.SOCKS5("tcp", u.Host, auth, &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("socks proxy %q: %w", raw, err)
	}
	return d, nil
}

// Describe renders the resolved setting for the startup log. Never prints
// credentials: userinfo is redacted.
func Describe(pc ProxyConfig) string {
	switch {
	case pc.Disabled:
		return "disabled (direct)"
	case pc.URL == "":
		return "none"
	}
	u, err := url.Parse(pc.URL)
	if err != nil {
		return "configured (unparseable)"
	}
	if u.User != nil {
		u.User = url.UserPassword("<redacted>", "")
		return u.String()
	}
	return u.String()
}
