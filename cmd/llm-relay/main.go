// llm-relay — OpenAI-compatible relay with per-key token metering, pricing
// and a cost dashboard. Single binary: proxy + JSON API + embedded SPA.
package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	assets "llm-relay"
	"llm-relay/internal/api"
	"llm-relay/internal/auth"
	"llm-relay/internal/config"
	"llm-relay/internal/db"
	"llm-relay/internal/fed"
	"llm-relay/internal/proxy"
	"llm-relay/internal/ratelimit"
	"llm-relay/internal/remote"
	"llm-relay/internal/store"
	"llm-relay/internal/usage"
)

// version is the build stamp, injected at link time with
// -ldflags "-X main.version=…" (make build, and the Dockerfile build stage
// via --build-arg VERSION=…). Empty in unstamped builds.
var version = ""

// buildVersion reports this build's version: the ldflags stamp if present,
// else the module version from the Go build info (set for
// `go install module@version` builds), else "dev". It is what the
// federation hello/hello_ack carries, so peers show what this relay runs.
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// fedOptions builds the federation manager options. The version is the
// only field main sets; everything else takes the production defaults.
func fedOptions() *fed.Options {
	return &fed.Options{Version: buildVersion()}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	// Headless connector mode (plan 2026-11-24-remote-backend-v1): the box
	// binds nothing and owns no state — its one job is the outbound channel
	// that exposes ENGINE_URL to a relay as a remote backend. No DB, no
	// listeners, no federation manager.
	if cfg.Headless {
		runHeadless(cfg)
		return
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

	// Federation channel singleton (plan 2026-11-22-relay-federation):
	// owns the hub registry + spoke dialer; Reload applies the stored
	// role. Deferred below, AFTER AttachServe: a dialer started here could
	// receive catalog frames whose tunnel would hit a mux with no frame
	// server mounted yet. fedOptions() stamps the build version into the
	// hello/hello_ack exchange so peers report real versions (#16).
	fedMgr := fed.NewManager(d, fedOptions())
	defer fedMgr.Close()

	// Remote-backend registry (plan 2026-11-24-remote-backend-v1): live
	// WebSocket channels for remote:// backends, accepted on any role.
	// The proxy tunnels remote:// traffic through it, the health cache
	// answers "online = live channel" from it, and the admin API mints
	// its connect tokens.
	remoteReg := remote.NewRegistry(d, nil, "", "")
	defer remoteReg.CloseAll("shutdown")

	apiSrv := &api.Server{DB: d, JWTSecret: cfg.JWTSecret, JWTTTL: 12 * time.Hour,
		RelayURL: cfg.PublicRelayURL, DashboardURL: cfg.PublicDashboardURL,
		TrustProxy: cfg.TrustProxyHeaders, SelfTLS: cfg.TLSPort != "0"}
	// Abuse-protection tiers (ticket #18), resolved from env with safe
	// non-zero defaults (0 per tier = that tier off). The api-side tiers
	// (login per-IP, login failure lockout, admin per-user) live behind
	// the Server's lazy limiters.
	lim := cfg.RateLimits()
	apiSrv.Limits = &lim
	apiSrv.Fed = fedMgr
	apiSrv.Remote = remoteReg
	ph := proxy.New(d, rec, cfg.TrustProxyHeaders)
	apiSrv.Flight = ph.Flight
	ph.RelayURL = cfg.PublicRelayURL
	ph.DashboardURL = cfg.PublicDashboardURL
	// Tier 3b: per-resolved-key bucket inside the proxy, after auth and
	// before any upstream work. The hook reuses the key id the metering
	// path already resolved (no second hash/lookup); bad keys never reach
	// it — they are bounded per source by the mux-level tier below.
	v1KeyStore := ratelimit.NewStore(lim.V1Key, ratelimit.StoreMax)
	ph.KeyLimit = func(keyID int64) (time.Duration, bool) {
		ok, wait := v1KeyStore.Allow("k:"+strconv.FormatInt(keyID, 10), time.Now())
		return wait, ok
	}
	// Federation seams on the proxy: the scheme-dispatching transport asks
	// the Manager to tunnel peer://hub:// backends, the health cache asks
	// it whether a synthetic backend's channel is up, and the Manager runs
	// tunneled requests back through this mux (its frame server refuses
	// any path outside /v1/).
	ph.Fed = fedMgr
	ph.Health.Synthetic = fedMgr.SyntheticOnline
	ph.Remote = remoteReg
	ph.Health.Remote = remoteReg.BackendOnline

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
	// Federation channel (plan 2026-11-22-relay-federation): server-token
	// (fed-) endpoints, machine-to-machine, on the public port beside the
	// sk-key surface — the spoke admin points hub_url at the hub's relay
	// URL. connect is the WS channel (task 2): PeerAuth verifies the
	// token on the upgrade request, the Manager role-gates (404 unless
	// hub) and runs the handshake. ping stays as the token-path probe.
	// Both are gated to TLS (plan 2026-11-24-remote-tls-v1): the fed-
	// token must never cross a network in the clear.
	relayMux.Handle("GET /federation/v1/ping", apiSrv.RequireTLS(apiSrv.PeerAuth(apiSrv.FederationPing)))
	relayMux.Handle("GET /federation/v1/connect", apiSrv.RequireTLS(apiSrv.PeerAuth(apiSrv.FederationConnect)))
	// Remote backends (plan 2026-11-24-remote-backend-v1): the connect
	// channel a headless box dials in on (rb- token). Not role-gated — a
	// remote backend is a backend, any relay can host one. TLS-gated for
	// the same reason as the fed endpoints.
	relayMux.Handle("GET /remote/v1/connect", apiSrv.RequireTLS(apiSrv.RemoteAuth(apiSrv.RemoteConnect)))
	relayMux.HandleFunc("POST /mcp", ph.MCP)
	// Tunneled (federation) requests are served from this same mux; the
	// fed frame server restricts them to /v1/* paths (see serve.go).
	fedMgr.AttachServe(relayMux)
	// Start federation only now that the tunnel's target mux is complete:
	// the stored role comes alive (spoke dialer starts, hub purges stale
	// materialized rows) with every route in place. A failed reconcile
	// (settings read or purge) is logged, not fatal: the relay serves
	// local traffic regardless, and the next settings PUT retries.
	if err := fedMgr.Reload(); err != nil {
		log.Printf("federation startup: %v", err)
	}

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
	adminMux.Handle("POST /api/admin/backends/remote", apiSrv.Admin(apiSrv.AdminCreateRemoteBackend))
	adminMux.Handle("POST /api/admin/backends/{id}/remote/rotate", apiSrv.Admin(apiSrv.AdminRotateRemoteToken))
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
	// Federation (plan 2026-11-22-relay-federation): self-service server
	// tokens (any user, My Keys page) + admin Servers-page management +
	// the instance role pair.
	adminMux.Handle("GET /api/peers", apiSrv.Auth(apiSrv.ListPeers))
	adminMux.Handle("POST /api/peers", apiSrv.Auth(apiSrv.CreatePeer))
	adminMux.Handle("POST /api/peers/{id}/rotate", apiSrv.Auth(apiSrv.RotateMyPeer))
	adminMux.Handle("DELETE /api/peers/{id}", apiSrv.Auth(apiSrv.DeleteMyPeer))
	adminMux.Handle("GET /api/admin/peers", apiSrv.Admin(apiSrv.AdminListPeers))
	adminMux.Handle("POST /api/admin/peers/{id}/rotate", apiSrv.Admin(apiSrv.AdminRotatePeer))
	adminMux.Handle("PATCH /api/admin/peers/{id}", apiSrv.Admin(apiSrv.AdminUpdatePeer))
	adminMux.Handle("DELETE /api/admin/peers/{id}", apiSrv.Admin(apiSrv.AdminDeletePeer))
	adminMux.Handle("GET /api/admin/federation", apiSrv.Admin(apiSrv.AdminGetFederation))
	adminMux.Handle("PUT /api/admin/federation", apiSrv.Admin(apiSrv.AdminPutFederation))
	adminMux.Handle("GET /api/admin/federation/stats", apiSrv.Admin(apiSrv.AdminFedStats))
	// Instance TLS certificate (plan 2026-11-24-remote-tls-v1): generate /
	// view / delete the self-signed cert the TLS listener serves and peers
	// pin.
	adminMux.Handle("GET /api/admin/tls", apiSrv.Admin(apiSrv.AdminGetTLS))
	adminMux.Handle("POST /api/admin/tls", apiSrv.Admin(apiSrv.AdminGenerateTLS))
	adminMux.Handle("DELETE /api/admin/tls", apiSrv.Admin(apiSrv.AdminDeleteTLS))
	adminMux.Handle("GET /api/federation/stats", apiSrv.Auth(apiSrv.FedStats))
	adminMux.Handle("GET /api/federation/role", apiSrv.Auth(apiSrv.FedRole))
	adminMux.Handle("GET /api/admin/usage/summary", apiSrv.Admin(apiSrv.UsageSummary))
	adminMux.Handle("GET /api/admin/usage/detail", apiSrv.Admin(apiSrv.UsageDetail))
	adminMux.Handle("GET /api/admin/live", apiSrv.Admin(apiSrv.Live))

	// Embedded SPA with index.html fallback for client-side routes.
	spa, err := fs.Sub(assets.Web, "web/dist")
	if err != nil {
		log.Fatalf("startup: embedded web assets missing: %v", err)
	}
	adminMux.Handle("/", spaHandler(spa))

	// Abuse-protection wrapping (ticket #18), applied in every mode —
	// standalone, hub and spoke all serve these muxes:
	//
	//   • Tier 3a: per-source-IP bucket on the public /v1 surface,
	//     pre-auth, so a bad-key flood (each attempt costs a hash + DB
	//     lookup before the 401) is bounded per client. Path-scoped so
	//     /healthz stays free and the machine channels keep their own
	//     stricter buckets; tunneled federation traffic is served through
	//     relayMux directly (the frame server calls the mux, not this
	//     wrapper), so it skips the per-IP tier and hits tier 3b on the
	//     synthetic key id (0) — ONE shared bucket for all tunneled traffic
	//     on this relay, not a per-peer one. Per-peer channel limits are
	//     tier 5, reserved for #17.
	//   • Tier 6: ONE server-wide in-flight shed shared by every listener
	//     (relay, admin, TLS): at saturation new requests get 503 +
	//     Retry-After instead of queueing onto a melting box. Long streams
	//     and live WS channels legitimately hold a slot for their whole
	//     duration — the cap sits far above normal concurrency (see
	//     ratelimit.Config). No blanket Read/WriteTimeout anywhere: SSE
	//     streaming must survive; per-handler deadlines bound the work.
	v1IPMW := ratelimit.Middleware(ratelimit.NewStore(lim.V1IP, ratelimit.StoreMax),
		func(r *http.Request) string { return ratelimit.SourceIP(r, cfg.TrustProxyHeaders) },
		"too many requests from this client, slow down")
	relayHandler := ratelimit.PathPrefix([]string{"/v1/"}, v1IPMW)(relayMux)
	shed := ratelimit.NewShed(lim.GlobalMax)
	relayHandler = shed.Middleware(relayHandler)
	adminHandler := shed.Middleware(adminMux)

	relaySrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           relayHandler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              cfg.AdminListenAddr,
		Handler:           adminHandler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	servers := []*http.Server{relaySrv}
	// Bind failures go to main (log.Fatalf here would skip the deferred
	// recorder/DB cleanup and leave the other port half-alive).
	srvErr := make(chan error, 3)
	go func() {
		log.Printf("relay API listening on %s", cfg.ListenAddr)
		if err := relaySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("relay listen: %w", err)
		}
	}()
	servers = append(servers, adminSrv)
	go func() {
		log.Printf("dashboard + admin API listening on %s", cfg.AdminListenAddr)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("admin listen: %w", err)
		}
	}()

	// TLS listener for machine channels (plan 2026-11-24-remote-tls-v1):
	// the same relay mux over TLS with the dashboard-generated cert.
	// GetCertificate reads the row per handshake — generate, rotate or
	// delete apply without restart, and a missing row fails handshakes
	// (deliberately: no silent fallback). TLS_PORT=0 skips the listener
	// entirely (the reverse-proxy deployment).
	if cfg.TLSPort != "0" {
		tlsSrv := &http.Server{
			Addr:              cfg.TLSListenAddr,
			Handler:           relayHandler,
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       120 * time.Second,
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					t, err := store.RemoteTLSGet(d)
					if errors.Is(err, store.ErrNotFound) {
						return nil, fmt.Errorf("no TLS certificate configured (generate one on the Settings page)")
					}
					if err != nil {
						return nil, fmt.Errorf("TLS certificate lookup failed: %w", err)
					}
					cert, err := tls.X509KeyPair([]byte(t.CertPEM), []byte(t.KeyPEM))
					if err != nil {
						return nil, fmt.Errorf("stored TLS certificate unusable: %w", err)
					}
					return &cert, nil
				},
			},
		}
		servers = append(servers, tlsSrv)
		go func() {
			log.Printf("relay API (TLS) listening on %s", cfg.TLSListenAddr)
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				srvErr <- fmt.Errorf("tls listen: %w", err)
			}
		}()
	}

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

// runHeadless is the whole headless box: dial the relay, expose the engine,
// redial forever. It returns only when SIGINT/SIGTERM arrives.
func runHeadless(cfg *config.Config) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("headless: %s → %s (engine %s)", cfg.RelayURL, "/remote/v1/connect", cfg.EngineURL)
	_ = remote.RunClient(ctx, remote.ClientConfig{
		ServerURL:   cfg.RelayURL,
		Token:       cfg.RelayToken,
		Pin:         cfg.RelayPin,
		EngineURL:   cfg.EngineURL,
		EngineToken: cfg.EngineToken,
	}, log.Printf)
	log.Println("bye")
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
