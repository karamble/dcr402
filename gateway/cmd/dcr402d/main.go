// Command dcr402d is the dcr402 payment gateway: a reverse proxy that
// fronts HTTP APIs and Streamable-HTTP MCP servers with Decred Lightning
// payment gating. See gateway/dcr402d.sample.yaml for configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/karamble/dcr402/gateway"
)

func main() {
	configPath := flag.String("config", "dcr402d.yaml", "path to the YAML configuration file")
	flag.Parse()

	cfg, err := gateway.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("dcr402d: %v", err)
	}
	gw, err := gateway.NewFromConfig(cfg)
	if err != nil {
		log.Fatalf("dcr402d: %v", err)
	}
	defer gw.Close()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the gateway proxies streaming (MCP SSE) responses.
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("dcr402d: listening on %s (%s, %d upstreams, credits=%v)",
			cfg.Listen, cfg.Network, len(cfg.Upstreams), cfg.Credits.Enabled)
		if cfg.TLSCert != "" {
			errCh <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("dcr402d: %v", err)
		}
	case s := <-sig:
		log.Printf("dcr402d: %v — shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("dcr402d: shutdown: %v", err)
		}
	}
}
