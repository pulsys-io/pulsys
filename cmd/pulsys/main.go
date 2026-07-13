// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Command pulsys is a Hugging Face disk-caching reverse proxy.
//
// Architecture:
//
//	listener
//	   |
//	coreserver.Server  ----- warm cache hit (sendfile + sf_hdtr fusion on Darwin)
//	   |                       2 allocs / op, 1 syscall on small bodies
//	   v
//	proxy.Handler.ServeHTTP   ----- everything else (cache miss, /_p/, metadata,
//	(net/http handler)                non-GET, redirects, errors)
//	   |
//	   v
//	upstream.Client (net/http transport)
//
// Listeners:
//
//	-listen        public ingress (HTTP/1.1, coreserver)
//	-admin-listen  Pulsys admin HTTP surface (auth, /admin/api/v1, /healthz, /metrics)
//	-pprof-listen  OPT-IN; net/http/pprof + expvar on a separate loopback port.
//	               Default disabled; pass e.g. -pprof-listen 127.0.0.1:6061
//	               in development/diagnosis only.  Never expose to the public
//	               internet.  See docs/security.md.
//
// Profiling (when -pprof-listen is set):
//
//	go tool pprof http://127.0.0.1:6061/debug/pprof/profile?seconds=30
//
// Example:
//
//	pulsys -listen :8080 -public-base-url http://127.0.0.1:8080 -cache-dir ./cache
//	HF_ENDPOINT=http://127.0.0.1:8080 hf download gpt2
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
	"github.com/pulsys-io/pulsys/internal/db"
	"github.com/pulsys-io/pulsys/internal/importer"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/observability"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// Build metadata, injected at release time by GoReleaser via -ldflags -X.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			_, _ = os.Stdout.WriteString("pulsys " + version + " (commit " + commit + ", built " + date + ")\n")
			return
		}
	}

	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	cfg, err := config.ParseFlags(fs, os.Args[1:])
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		fs.Usage()
		os.Exit(2)
	}

	log := logx.New(cfg.LogLevel)
	telemetry.Register()

	// Pulsys has no open mode: it authenticates to Hugging Face with its
	// own read-only token on every cold-miss read and never forwards a
	// caller's credential upstream, so the token is mandatory.
	if cfg.UpstreamHFToken == "" {
		log.Error("PULSYS_HF_TOKEN is required: Pulsys uses its own read-only Hugging Face token for all upstream reads (cold misses + imports) and never forwards client tokens")
		os.Exit(1)
	}

	// Optional: PULSYS_MEMPROFILE_RATE=N in bytes between sampled
	// allocations.  Setting to 1 captures every allocation -- expensive
	// but invaluable for proving the warm-hit floor.  Use only in
	// benchmarking + diagnosis runs.
	if v := os.Getenv("PULSYS_MEMPROFILE_RATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			runtime.MemProfileRate = n
			log.Info("MemProfileRate", "bytes", n)
		}
	}

	store, err := cache.NewStoreWithOptions(cfg.CacheDir, cfg.FsyncPolicy, cache.StoreOptions{
		MaxBytes: cfg.CacheMaxBytes,
	})
	if err != nil {
		log.Error("cache init", "err", err)
		os.Exit(1)
	}

	up := upstream.New(cfg)
	h := proxy.NewHandler(cfg, store, up, log)
	loopback := importer.NewLoopbackProxy(cfg, store, up, log)
	defer loopback.Close()

	// Default admin handler is a minimal dev-mode 404+healthz
	// surface: it serves /healthz so liveness probes work, and
	// When PULSYS_DB_DSN is set we mount admin.CombinedHandler for the
	// full Pulsys admin API.  Critically, neither variant exposes
	// /debug/pprof or /debug/vars: those now live on the opt-in
	// -pprof-listen address (see below).
	var adminHandler http.Handler
	var pgPool *db.Pool
	var dataPlaneGate coreserver.AuthGate
	var riverBundle *importer.RiverBundle
	// The admin plane is mandatory: Pulsys has no open mode.  Without a
	// database there is no way to issue or enforce the Pulsys API keys
	// that every data-plane request requires, so refuse to start.
	dsn := os.Getenv("PULSYS_DB_DSN")
	if dsn == "" {
		log.Error("PULSYS_DB_DSN is required: Pulsys has no open mode; the admin plane (Postgres) must be configured to issue and enforce API keys")
		os.Exit(1)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AppName: "pulsys-admin"})
		cancel()
		if err != nil {
			log.Error("postgres init", "err", err)
			os.Exit(1)
		}
		pgPool = pool
		tenantName := os.Getenv("PULSYS_TENANT")
		if tenantName == "" {
			tenantName = "default"
		}
		// PAT enforcement on the data plane.  Every request (warm hit
		// AND fallback) is checked against the tokens table before
		// the cache or upstream is consulted.  Phase 5 wires the
		// gate into admin.Config.PATCache so the admin revoke
		// handler punches a hole in the local cache the instant
		// a token's revoked_at is written -- closing the residual
		// 60-second PositiveTTL admit window that the original
		// 2026-05-21 incident chained off.
		// /healthz, /readyz, /metrics bypass; everything else
		// requires "Bearer pulsys_..." that LookupAPIToken accepts.
		patGate := auth.NewPATGate(authstore.NewPG(pool.Pgx()))
		dataPlaneGate = patGate

		migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := db.MigrateRiverPool(migrateCtx, pool.Pgx()); err != nil {
			migrateCancel()
			log.Error("river migrate", "err", err)
			os.Exit(1)
		}
		migrateCancel()

		importWorkers := envInt("PULSYS_IMPORT_MAX_WORKERS", runtime.GOMAXPROCS(0)*2)
		if importWorkers < 1 {
			importWorkers = 1
		}
		importJobTimeout := envDuration("PULSYS_IMPORT_JOB_TIMEOUT", 24*time.Hour)
		workerCtx := context.Background()
		riverBundle, err = importer.NewRiverBundle(workerCtx, importer.RiverConfig{
			Pool:            pool.Pgx(),
			LoopbackBaseURL: loopback.URL(),
			HFToken:         cfg.UpstreamHFToken,
			MaxWorkers:      importWorkers,
			WorkerEnabled:   importWorkerEnabled(),
			JobTimeout:      importJobTimeout,
		})
		if err != nil {
			log.Error("river init", "err", err)
			os.Exit(1)
		}
		if importWorkerEnabled() {
			log.Info("import worker enabled", "loopback", loopback.URL(), "fetch_workers", importWorkers, "job_timeout", importJobTimeout)
		} else {
			log.Info("import worker disabled", "loopback", loopback.URL())
		}

		secureCookies := cfg.PublicBaseURL.Scheme == "https"
		adminHandler = admin.CombinedHandler(admin.Config{
			Pool:          pool.Pgx(),
			DBPool:        pool,
			CacheDir:      cfg.CacheDir,
			Cache:         store,
			TenantName:    tenantName,
			SecureCookies: secureCookies,
			Metrics:       observability.NewRegistry(),
			PATCache:      patGate,
			RiverClient:   riverBundle.Client,
		})
		log.Info("admin API enabled", "tenant", tenantName)
		log.Info("PAT enforcement enabled on data plane")
	}

	// Defense in depth: the mandatory DSN above guarantees dataPlaneGate is
	// set here.  The public server below is additionally built with
	// RequireAuth=true so that, should a future refactor ever drop the gate,
	// the data plane fails closed (rejects every request) instead of
	// silently serving an unauthenticated cache.

	adminSrv := &http.Server{Addr: cfg.AdminListen, Handler: adminHandler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("admin server", "addr", cfg.AdminListen)
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("admin server", "err", err)
		}
	}()

	// Opt-in pprof listener: empty -pprof-listen disables pprof
	// entirely.  When set, bind a separate http.Server that mounts
	// net/http/pprof (registered on http.DefaultServeMux by the
	// blank import) plus expvar.  Always bind on loopback only
	// even when the operator passes a non-loopback address: pprof
	// + expvar leak heap, goroutine state, the full process
	// environment, and command line, and must never be reachable
	// from the public internet.
	var pprofSrv *http.Server
	if cfg.PprofListen != "" {
		pprofSrv = startPprof(cfg.PprofListen, log)
	}

	listenerCount := cfg.Listeners
	if listenerCount <= 0 {
		listenerCount = 1
	}
	lns, err := coreserver.NewReuseportListeners("tcp", cfg.Listen, listenerCount)
	if err != nil {
		log.Error("listen", "addr", cfg.Listen, "count", listenerCount, "err", err)
		os.Exit(1)
	}

	core := &coreserver.Server{
		Cfg:               cfg,
		Store:             store,
		Fallback:          coreserver.HandlerFallback(h),
		AuthGate:          dataPlaneGate,
		RequireAuth:       true,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		MaxConnsPerIP:     cfg.MaxConnsPerIP,
		SocketSendBuf:     cfg.SocketSendBuf,
		TCPCork:           cfg.TCPCork,
		IoUring:           cfg.IoUring,
	}

	go func() {
		log.Info("proxy listening", "addr", lns[0].Addr().String(), "engine", "coreserver", "listeners", len(lns))
		if err := core.ServeMulti(lns...); err != nil {
			log.Error("coreserver serve", "err", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if riverBundle != nil {
		if err := riverBundle.Stop(ctx); err != nil {
			log.Error("river stop", "err", err)
		}
	}
	_ = adminSrv.Shutdown(ctx)
	if pprofSrv != nil {
		_ = pprofSrv.Shutdown(ctx)
	}
	if pgPool != nil {
		pgPool.Close()
	}
	core.Close()
	for _, ln := range lns {
		_ = ln.Close()
	}
	log.Info("shutdown complete")
}

func importWorkerEnabled() bool {
	v := strings.TrimSpace(os.Getenv("PULSYS_IMPORT_WORKER"))
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	default:
		return envBool("PULSYS_IMPORT_WORKER")
	}
}

func envBool(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(name string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
