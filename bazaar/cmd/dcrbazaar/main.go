// Command dcrbazaar is the dcr402 payment facilitator: the standard x402 v2
// facilitator endpoints (/verify, /settle, /supported) plus a Bazaar-style
// discovery index of DCR-payable resources. It runs non-custodial (trust
// topology T2) and never holds keys or funds. See dcrbazaar.sample.yaml for
// configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/karamble/dcr402/bazaar"
)

func main() {
	configPath := flag.String("config", "dcrbazaar.yaml", "path to the YAML configuration file")
	flag.Parse()

	cfg, err := bazaar.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("dcrbazaar: %v", err)
	}

	store, err := openStore(cfg.Store)
	if err != nil {
		log.Fatalf("dcrbazaar: %v", err)
	}
	defer store.Close()

	svc, err := bazaar.New(*cfg, store)
	if err != nil {
		log.Fatalf("dcrbazaar: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("dcrbazaar: listening on %s (networks=%s, discovery=%v, auth=%v)",
			cfg.Listen, strings.Join(cfg.Networks, ","), cfg.Discovery.Enabled, len(cfg.APIKeys) > 0)
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
			log.Fatalf("dcrbazaar: %v", err)
		}
	case s := <-sig:
		log.Printf("dcrbazaar: %v - shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("dcrbazaar: shutdown: %v", err)
		}
	}
}

// openStore returns a SQLite store at path, or an in-memory store when path is
// empty (state is not persisted across restarts).
func openStore(path string) (bazaar.Store, error) {
	if strings.TrimSpace(path) == "" {
		log.Print("dcrbazaar: no store path configured; using in-memory store (not persisted)")
		return bazaar.NewMemory(), nil
	}
	return bazaar.OpenSQLite(path)
}
