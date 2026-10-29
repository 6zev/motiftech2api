// Command motiftech2api is an OpenAI-compatible gateway for chat.motiftech.io.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/6zev/motiftech2api/internal/config"
	"github.com/6zev/motiftech2api/internal/netx"
	"github.com/6zev/motiftech2api/internal/server"
	"github.com/6zev/motiftech2api/internal/store"
	"github.com/6zev/motiftech2api/internal/update"
)

// version is injected at build time:
//
//	go build -ldflags "-X main.version=v1.2.3"
//
// It stays "dev" for a plain local build.
var version = "dev"

func main() {
	// Load .env before anything reads the environment. Real environment variables
	// win, so systemd units and docker --env-file are never shadowed.
	dotenvPath := os.Getenv("DOTENV_PATH")
	if dotenvPath == "" {
		dotenvPath = config.DefaultDotEnvPath()
	}
	applied, found := config.ApplyDotEnv(dotenvPath)

	cfg := config.Load()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "-v":
			fmt.Println(version)
			return
		case "--update":
			os.Exit(runUpdate(cfg))
		case "--check-update":
			os.Exit(runUpdateCheck(cfg))
		case "--help", "-h", "-help":
			printUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q\n\n", os.Args[1])
			printUsage()
			os.Exit(2)
		}
	}

	// One outbound client for both the upstream and the updater, so proxy
	// settings apply consistently.
	httpClient, err := netx.NewClient(time.Duration(cfg.UpstreamTimeoutMS) * time.Millisecond)
	if err != nil {
		log.Fatalf("outbound client: %v", err)
	}

	st := store.New(cfg.StorePath, time.Duration(cfg.ConversationTTLHours)*time.Hour)
	srv := server.NewWithClient(cfg, st, httpClient)

	httpSrv := &http.Server{
		Addr:              cfg.Host + ":" + itoa(cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: responses are SSE and may legitimately run for minutes.
	}

	switch {
	case found && len(applied) > 0:
		log.Printf("env: %s (%d setting(s): %s)",
			config.DescribeDotEnvPath(dotenvPath), len(applied), strings.Join(applied, ", "))
	case found:
		log.Printf("env: %s (no new settings; real environment already set them)",
			config.DescribeDotEnvPath(dotenvPath))
	default:
		log.Printf("env: no %s found, using the environment only",
			config.DescribeDotEnvPath(dotenvPath))
	}

	log.Printf("motif gateway listening on http://%s:%d", cfg.Host, cfg.Port)
	log.Printf("upstream: %s", cfg.UpstreamBase)
	log.Printf("proxy: %s", netx.Describe(netx.Resolve()))
	if len(cfg.GatewayAPIKeys) == 0 {
		log.Printf("auth: disabled (set GATEWAY_API_KEYS to require a bearer key)")
	} else {
		log.Printf("auth: %d key(s)", len(cfg.GatewayAPIKeys))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// updateOptions builds the updater configuration from the environment. The
// outbound proxy settings apply here too, so updating works behind the same
// network that the gateway needs.
func updateOptions() (update.Options, error) {
	hc, err := netx.NewClient(5 * time.Minute)
	if err != nil {
		return update.Options{}, err
	}
	target, err := os.Executable()
	if err != nil {
		return update.Options{}, fmt.Errorf("locate the running binary: %w", err)
	}
	repo := strings.TrimSpace(os.Getenv("UPDATE_REPO"))
	if repo == "" {
		repo = update.DefaultRepo
	}
	return update.Options{
		Repo:       repo,
		APIBase:    os.Getenv("UPDATE_API_BASE"),
		CurrentVer: version,
		HTTP:       hc,
		Target:     target,
		AllowDraft: os.Getenv("UPDATE_ALLOW_DRAFT") == "1",
		AllowPre:   os.Getenv("UPDATE_ALLOW_PRERELEASE") == "1",
		Token:      firstNonEmpty(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN")),
		Out:        os.Stdout,
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func runUpdate(cfg config.Config) int {
	_ = cfg
	opts, err := updateOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 1
	}
	fmt.Printf("checking %s for updates (proxy: %s)\n", opts.Repo, netx.Describe(netx.Resolve()))

	res, err := update.Run(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "the existing binary at %s was not changed\n", opts.Target)
		return 1
	}
	if !res.Updated {
		fmt.Printf("already up to date (%s)\n", res.From)
		return 0
	}
	fmt.Printf("updated %s -> %s\n", res.From, res.To)
	fmt.Printf("replaced %s\n", res.Path)
	fmt.Println("restart the service to use the new version")
	return 0
}

// runUpdateCheck reports whether a newer release exists, without changing anything.
func runUpdateCheck(cfg config.Config) int {
	_ = cfg
	opts, err := updateOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "update check: %v\n", err)
		return 1
	}
	res, err := update.Run(context.Background(), update.Options{
		Repo:       opts.Repo,
		APIBase:    opts.APIBase,
		CurrentVer: opts.CurrentVer,
		HTTP:       opts.HTTP,
		Target:     opts.Target,
		AllowDraft: opts.AllowDraft,
		AllowPre:   opts.AllowPre,
		Token:      opts.Token,
		Out:        io.Discard,
		CheckOnly:  true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "update check failed: %v\n", err)
		return 1
	}
	if res.Updated {
		fmt.Printf("update available: %s -> %s\n", res.From, res.To)
		return 0
	}
	fmt.Printf("up to date (%s)\n", res.From)
	return 0
}

func printUsage() {
	cfg := config.Load()
	fmt.Printf(`motiftech2api %s
An OpenAI-compatible gateway for chat.motiftech.io.

Usage:
  motiftech2api              start the server
  motiftech2api --update     replace this binary with the latest release
  motiftech2api --check-update   report whether a newer release exists
  motiftech2api --version    print the version
  motiftech2api --help       print this help

Updating (--update):
  UPDATE_REPO            %s
  UPDATE_API_BASE        %s
  UPDATE_ALLOW_PRERELEASE  set 1 to accept a pre-release
  UPDATE_ALLOW_DRAFT     set 1 to accept a draft
  GITHUB_TOKEN / GH_TOKEN  optional, raises the API rate limit

Outbound proxy (applies to the upstream and to --update):
  MOTIF_PROXY            explicit proxy; http(s):// or socks5://; "none" to disable
  HTTPS_PROXY/HTTP_PROXY/ALL_PROXY  honoured when MOTIF_PROXY is unset
  NO_PROXY               standard bypass list

Configured through the environment:
  PORT                  %d
  HOST                  %s
  UPSTREAM_BASE         %s
  UPSTREAM_TIMEOUT_MS   %d
  GATEWAY_API_KEYS      %s
  CONVERSATION_TTL_HOURS %d
  STORE_PATH            %s
  DEBUG                 %v
  DOTENV_PATH           %s  (read at startup; real env wins)

Endpoints: GET /health, GET /v1/models, POST /v1/chat/completions
`, version, updateRepoForUsage(), updateAPIBaseForUsage(), cfg.Port, cfg.Host,
		cfg.UpstreamBase, cfg.UpstreamTimeoutMS,
		describeKeys(len(cfg.GatewayAPIKeys)), cfg.ConversationTTLHours,
		cfg.StorePath, cfg.Debug, dotenvPathForUsage())
}

func updateRepoForUsage() string {
	if r := strings.TrimSpace(os.Getenv("UPDATE_REPO")); r != "" {
		return r
	}
	return update.DefaultRepo
}

func updateAPIBaseForUsage() string {
	if b := strings.TrimSpace(os.Getenv("UPDATE_API_BASE")); b != "" {
		return b
	}
	return update.DefaultAPIBase
}

func dotenvPathForUsage() string {
	if p := os.Getenv("DOTENV_PATH"); p != "" {
		return p
	}
	return ".env"
}

func describeKeys(n int) string {
	if n == 0 {
		return "(none: auth disabled)"
	}
	return fmt.Sprintf("%d key(s)", n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
