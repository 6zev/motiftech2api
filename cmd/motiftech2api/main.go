// Command motiftech2api is an OpenAI-compatible gateway for chat.motiftech.io.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/6zev/motiftech2api/internal/config"
	"github.com/6zev/motiftech2api/internal/server"
	"github.com/6zev/motiftech2api/internal/store"
)

// version is injected at build time:
//
//	go build -ldflags "-X main.version=v1.2.3"
//
// It stays "dev" for a plain local build.
var version = "dev"

func main() {
	cfg := config.Load()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "-v":
			fmt.Println(version)
			return
		case "--help", "-h", "-help":
			printUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q\n\n", os.Args[1])
			printUsage()
			os.Exit(2)
		}
	}

	st := store.New(cfg.StorePath, time.Duration(cfg.ConversationTTLHours)*time.Hour)
	srv := server.New(cfg, st)

	httpSrv := &http.Server{
		Addr:              cfg.Host + ":" + itoa(cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: responses are SSE and may legitimately run for minutes.
	}

	log.Printf("motif gateway listening on http://%s:%d", cfg.Host, cfg.Port)
	log.Printf("upstream: %s", cfg.UpstreamBase)
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

func printUsage() {
	cfg := config.Load()
	fmt.Printf(`motiftech2api %s
An OpenAI-compatible gateway for chat.motiftech.io.

Usage:
  motiftech2api              start the server
  motiftech2api --version    print the version
  motiftech2api --help       print this help

Configured through the environment:
  PORT                  %d
  HOST                  %s
  UPSTREAM_BASE         %s
  UPSTREAM_TIMEOUT_MS   %d
  GATEWAY_API_KEYS      %s
  CONVERSATION_TTL_HOURS %d
  STORE_PATH            %s
  DEBUG                 %v

Endpoints: GET /health, GET /v1/models, POST /v1/chat/completions
`, version, cfg.Port, cfg.Host, cfg.UpstreamBase, cfg.UpstreamTimeoutMS,
		describeKeys(len(cfg.GatewayAPIKeys)), cfg.ConversationTTLHours,
		cfg.StorePath, cfg.Debug)
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
