// llm-relay — OpenAI-compatible relay with per-key token metering, pricing
// and a cost dashboard. Single binary: proxy + JSON API + embedded SPA.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	assets "llm-relay"
	"llm-relay/internal/api"
	"llm-relay/internal/auth"
	"llm-relay/internal/config"
	"llm-relay/internal/db"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
	"llm-relay/internal/usage"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	d, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		log.Fatalf("startup: %v", err)
	}
	bootstrapAdmin(d, cfg)

	rec := usage.NewRecorder(d)
	rec.Start()

	apiSrv := &api.Server{DB: d, JWTSecret: cfg.JWTSecret, JWTTTL: 12 * time.Hour,
		RelayURL: cfg.PublicRelayURL, DashboardURL: cfg.PublicDashboardURL}
	ph := proxy.New(d, rec, cfg.TrustProxyHeaders)
	apiSrv.Flight = ph.Flight
	ph.RelayURL = cfg.PublicRelayURL
	ph.DashboardURL = cfg.PublicDashboardURL

	healthz := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}

	// Public relay port: sk-key authenticated OpenAI/Anthropic endpoints.
	relayMux := http.NewServeMux()
	relayMux.HandleFunc("GET /healthz", healthz)
	relayMux.HandleFunc("POST /v1/chat/completions", ph.Chat)
	relayMux.HandleFunc("POST /v1/completions", ph.Completions)
	relayMux.HandleFunc("POST /v1/responses", ph.Responses)
	relayMux.HandleFunc("POST /v1/messages/count_tokens", ph.CountTokens)
	relayMux.HandleFunc("POST /v1/messages", ph.Messages)
	relayMux.HandleFunc("POST /v1/embeddings", ph.Embeddings)
	relayMux.HandleFunc("GET /v1/models", ph.Models)
	relayMux.HandleFunc("GET /v1/agent-guide", ph.AgentGuide)
	relayMux.HandleFunc("GET /v1/utilization", ph.UtilizationREST)
	relayMux.HandleFunc("POST /mcp", ph.MCP)

	// Dashboard port: JWT API + embedded SPA. This is the port to put
	// behind the reverse proxy.
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /healthz", healthz)
	adminMux.Handle("POST /api/auth/login", apiSrv.RateLimitLogin(apiSrv.Login))
	adminMux.Handle("GET /api/me", apiSrv.Auth(apiSrv.Me))
	adminMux.Handle("PATCH /api/me", apiSrv.Auth(apiSrv.UpdateMe))
	adminMux.Handle("POST /api/auth/change_password", apiSrv.Auth(apiSrv.ChangePassword))
	adminMux.Handle("GET /api/info", apiSrv.Auth(apiSrv.Info))
	adminMux.Handle("GET /api/models", apiSrv.Auth(apiSrv.PublicModels))
	adminMux.Handle("GET /api/keys", apiSrv.Auth(apiSrv.ListKeys))
	adminMux.Handle("POST /api/keys", apiSrv.Auth(apiSrv.CreateKey))
	adminMux.Handle("DELETE /api/keys/{id}", apiSrv.Auth(apiSrv.RevokeKey))
	adminMux.Handle("GET /api/usage/summary", apiSrv.Auth(apiSrv.UsageSummary))
	adminMux.Handle("GET /api/usage/detail", apiSrv.Auth(apiSrv.UsageDetail))
	adminMux.Handle("GET /api/live", apiSrv.Auth(apiSrv.Live))

	adminMux.Handle("GET /api/admin/users", apiSrv.Admin(apiSrv.AdminListUsers))
	adminMux.Handle("POST /api/admin/users", apiSrv.Admin(apiSrv.AdminCreateUser))
	adminMux.Handle("PATCH /api/admin/users/{id}", apiSrv.Admin(apiSrv.AdminUpdateUser))
	adminMux.Handle("GET /api/admin/backends", apiSrv.Admin(apiSrv.AdminListBackends))
	adminMux.Handle("POST /api/admin/backends", apiSrv.Admin(apiSrv.AdminCreateBackend))
	adminMux.Handle("PATCH /api/admin/backends/{id}", apiSrv.Admin(apiSrv.AdminUpdateBackend))
	adminMux.Handle("DELETE /api/admin/backends/{id}", apiSrv.Admin(apiSrv.AdminDeleteBackend))
	adminMux.Handle("POST /api/admin/backends/{id}/probe", apiSrv.Admin(apiSrv.AdminProbeBackend))
	adminMux.Handle("POST /api/admin/backends/{id}/models", apiSrv.Admin(apiSrv.AdminAddBackendModels))
	adminMux.Handle("GET /api/admin/models", apiSrv.Admin(apiSrv.AdminListModels))
	adminMux.Handle("PATCH /api/admin/models/{id}", apiSrv.Admin(apiSrv.AdminUpdateModel))
	adminMux.Handle("POST /api/admin/models/{id}/benchmark", apiSrv.Admin(apiSrv.AdminBenchmarkModel))
	adminMux.Handle("DELETE /api/admin/models/{id}", apiSrv.Admin(apiSrv.AdminDeleteModel))
	adminMux.Handle("GET /api/admin/groups", apiSrv.Admin(apiSrv.AdminListGroups))
	adminMux.Handle("POST /api/admin/groups", apiSrv.Admin(apiSrv.AdminCreateGroup))
	adminMux.Handle("GET /api/admin/groups/{id}", apiSrv.Admin(apiSrv.AdminGetGroup))
	adminMux.Handle("PATCH /api/admin/groups/{id}", apiSrv.Admin(apiSrv.AdminUpdateGroup))
	adminMux.Handle("DELETE /api/admin/groups/{id}", apiSrv.Admin(apiSrv.AdminDeleteGroup))
	adminMux.Handle("GET /api/admin/settings", apiSrv.Admin(apiSrv.AdminGetSettings))
	adminMux.Handle("PUT /api/admin/settings", apiSrv.Admin(apiSrv.AdminPutSettings))
	adminMux.Handle("GET /api/admin/usage/summary", apiSrv.Admin(apiSrv.UsageSummary))
	adminMux.Handle("GET /api/admin/usage/detail", apiSrv.Admin(apiSrv.UsageDetail))
	adminMux.Handle("GET /api/admin/live", apiSrv.Admin(apiSrv.Live))

	// Embedded SPA with index.html fallback for client-side routes.
	spa, err := fs.Sub(assets.Web, "web/dist")
	if err != nil {
		log.Fatalf("startup: embedded web assets missing: %v", err)
	}
	adminMux.Handle("/", spaHandler(spa))

	relaySrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           relayMux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              cfg.AdminListenAddr,
		Handler:           adminMux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	servers := []*http.Server{relaySrv, adminSrv}
	// Bind failures go to main (log.Fatalf here would skip the deferred
	// recorder/DB cleanup and leave the other port half-alive).
	srvErr := make(chan error, 2)
	go func() {
		log.Printf("relay API listening on %s", cfg.ListenAddr)
		if err := relaySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("relay listen: %w", err)
		}
	}()
	go func() {
		log.Printf("dashboard + admin API listening on %s", cfg.AdminListenAddr)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("admin listen: %w", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var fatal error
	select {
	case <-ctx.Done():
		log.Println("shutting down…")
	case fatal = <-srvErr:
		log.Printf("server failed: %v — shutting down", fatal)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		// DeadlineExceeded is normal when long LLM streams outlive the
		// grace period; the recorder tolerates their late submits.
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
	rec.Close(5 * time.Second)
	log.Println("bye")
	if fatal != nil {
		os.Exit(1)
	}
}

// spaHandler serves static files and falls back to index.html for any path
// that is not an API route and not an existing asset.
func spaHandler(root fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path, "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(root, name); err != nil {
			// Unknown path: SPA shell for route-like paths, 404 for assets.
			if strings.Contains(path[strings.LastIndex(path, "/"):], ".") {
				http.NotFound(w, r)
				return
			}
			shell, err := fs.ReadFile(root, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(shell)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// bootstrapAdmin creates the first admin account from env on an empty DB.
func bootstrapAdmin(d *sql.DB, cfg *config.Config) {
	n, err := store.UserCount(d)
	if err != nil {
		log.Fatalf("startup: user count: %v", err)
	}
	if n > 0 || cfg.AdminUsername == "" || cfg.AdminPassword == "" {
		return
	}
	hash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		log.Fatalf("startup: admin hash: %v", err)
	}
	if _, err := store.CreateUser(d, cfg.AdminUsername, "", hash, "admin"); err != nil {
		log.Fatalf("startup: admin create: %v", err)
	}
	log.Printf("bootstrap admin account created: %s", cfg.AdminUsername)
}
