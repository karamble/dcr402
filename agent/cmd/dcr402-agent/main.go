// Command dcr402-agent is the agent-side wallet/policy daemon: it exposes
// Decred payment tools over MCP (stdio and/or streamable HTTP) so any
// MCP-capable agent can spend within hard, owner-set policy, and serves an
// optional web dashboard for status and human approvals. It is one thin
// consumer of the github.com/karamble/dcr402/agent packages — see that
// module to embed the pieces directly.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/karamble/dcr402/agent"
	"github.com/karamble/dcr402/agent/mcpserver"
	"github.com/karamble/dcr402/agent/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("dcr402-agent: ")
	flag.Parse()

	cfg := agent.FromEnv()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	daemon, err := agent.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer daemon.Close()

	// SIGHUP → policy reload.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := daemon.Reload(); err != nil {
				log.Printf("policy reload failed (keeping current): %v", err)
			} else {
				log.Print("policy reloaded")
			}
		}
	}()

	go func() {
		if err := daemon.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("daemon: %v", err)
		}
	}()

	mcpSrv := mcpserver.New(daemon.Service, cfg.Network)

	// HTTP transport (MCP streamable + web dashboard) when requested.
	var httpSrv *http.Server
	if cfg.MCP == agent.MCPHTTP || cfg.MCP == agent.MCPBoth {
		// Fail closed: the /mcp tool surface and the whole /api can spend
		// money and resolve approvals, so they must be authenticated. If the
		// operator did not set a token, generate one and log it once.
		token := cfg.HTTPToken
		if token == "" {
			token = randomToken()
			log.Printf("no DCR402_AGENT_HTTP_TOKEN set; generated one for this run: %s", token)
			log.Print("pass it as `Authorization: Bearer <token>` to the MCP client and dashboard")
		}
		webSrv, err := web.New(web.Config{
			Service: daemon.Service,
			Network: cfg.Network,
			Token:   token,
		})
		if err != nil {
			log.Fatal(err)
		}
		mux := http.NewServeMux()
		mcpHandler := mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return mcpSrv.MCP() }, nil)
		mux.Handle("/mcp", bearer(token, mcpHandler))
		mux.Handle("/mcp/", bearer(token, mcpHandler))
		// The web dashboard (and its JSON API) mounts at the root — delete
		// these two lines to ship the daemon with no UI.
		mux.Handle("/", webSrv.Handler())

		httpSrv = &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      120 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			log.Printf("listening on %s (MCP /mcp, dashboard /)", cfg.HTTPAddr)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("http: %v", err)
			}
		}()
	}

	// stdio transport (blocks until the client disconnects or ctx ends).
	if cfg.MCP == agent.MCPStdio || cfg.MCP == agent.MCPBoth {
		log.Print("serving MCP over stdio")
		if err := mcpSrv.MCP().Run(ctx, &mcp.StdioTransport{}); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Printf("stdio: %v", err)
		}
		stop() // stdio ended → shut the daemon down
	}

	<-ctx.Done()
	log.Print("shutting down")
	if httpSrv != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(sctx)
		cancel()
	}
}

// bearer guards a handler with a static token. It fails closed: an empty
// token rejects every request.
func bearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// randomToken returns a fresh 24-byte hex token for authenticating the HTTP
// surface when the operator did not supply one.
func randomToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatalf("generating auth token: %v", err)
	}
	return hex.EncodeToString(b[:])
}
