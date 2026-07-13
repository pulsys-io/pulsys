// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package testserver

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/registry"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// envIoUring is the test-time switch that flips the coreserver
// into io_uring mode for the entire process.  Set to a truthy
// value ("1", "true", "yes") in the docker compose runner so
// the security test matrix exercises the Linux iouring code
// path (parser_linux.go, iouring_reactor_linux.go,
// iouring_ring_linux.go) instead of the cork+sendfile path.
//
// Linux-only.  On Darwin (where the security tests also run as
// a smoke check), the IoUring flag is ignored by the coreserver
// constructor via the //go:build tags on iouring_*_linux.go;
// flipping the env var on macOS is a no-op rather than an
// error, which keeps the same env var safe to bake into CI.
const envIoUring = "PULSYS_TEST_IOURING"

// envIoUringRequire instructs the harness to FAIL the test if
// io_uring was requested but the kernel rejected the setup
// (e.g. < 6.1 or missing CAP_SYS_NICE).  Without this flag,
// the coreserver silently falls back to cork+sendfile which
// would let a "I built a kernel-too-old image" mistake pass
// CI green.  Set to a truthy value alongside envIoUring inside
// the security-tests-linux container.
const envIoUringRequire = "PULSYS_TEST_IOURING_REQUIRE"

// envTruthy reports whether the named env var is set to a
// truthy value.  Empty, "0", "false", "no" all => false.
func envTruthy(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch v {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// Config controls the test stack. Zero value is valid (defaults
// applied) but tests usually want to seed repos via Mock.
type Config struct {
	// Mock holds the mock Hub configuration. Defaults: no repos, no
	// auth requirement.
	Mock mockhub.Config

	// CacheDir; defaults to t.TempDir()+"/cache".
	CacheDir string

	// FsyncPolicy for the cache. Defaults to "none" (fastest, fine for
	// tests).
	FsyncPolicy string

	// LogLevel for the proxy's logger. Defaults to "warn" so noise
	// during normal test runs is low; set to "debug" for diagnosis.
	LogLevel string

	// Offline forces the proxy into offline mode (no upstream calls).
	// Useful for asserting "no egress after warm" invariants.
	Offline bool

	// ExtraAllowHosts is appended to the proxy's allow-host list.
	// The mock Hub host is always allowed.
	ExtraAllowHosts []string

	// Registry configures the Pulsys self-hosted registry layer.
	// When nil, no registry handler is mounted and the stack behaves
	// exactly like the v1 pre-registry proxy. When non-nil, the
	// registry handler is the outer layer (registry > mirror >
	// passthrough).
	Registry *RegistryConfig

	// AuthGate, when non-nil, is installed on the coreserver and runs
	// before every request (warm hit and fallback alike).  Tests that
	// exercise the production PAT enforcement path pass an
	// auth.PATGate (or any other implementation) here; tests that
	// only care about caching / proxy behavior leave it nil and the
	// data plane is permissive, matching cmd/pulsys/main.go when
	// PULSYS_DB_DSN is not configured.
	AuthGate coreserver.AuthGate

	// IoUring forces io_uring mode on the coreserver for THIS
	// stack regardless of the global PULSYS_TEST_IOURING env
	// var.  Most callers leave this false and let the env var
	// drive the choice from outside the test (see
	// docker-compose.security-tests.yml).  Set this explicitly
	// in a Linux-only test that needs io_uring even when CI is
	// run without the env var.  Ignored on Darwin via the
	// coreserver's build-tagged stubs.
	IoUring bool

	// Slowloris-defense overrides for the coreserver.  Zero
	// values leave the coreserver's own defaults intact
	// (IdleTimeout=60s, ReadHeaderTimeout=5s, MaxConnsPerIP=0).
	// Tests that need to assert the defense without waiting on
	// real-time deadlines pass tiny values here (e.g. 200ms)
	// and rely on net.Conn deadline semantics being honored by
	// loopback.
	IdleTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	MaxConnsPerIP     int
}

// RegistryConfig wires a registry layer into the test stack.
type RegistryConfig struct {
	// Store is the registry data layer. The harness only constructs
	// this when a Postgres pool was provided by the caller; tests
	// that need registry behavior pass a pre-built *registry.Store.
	Store *registry.Store
	// Blobs is the content-addressed object store backing the
	// registry's bytes.
	Blobs blobstore.Store
	// TenantID identifies the single tenant the registry serves.
	TenantID string
	// AuditExecer (optional) enables audit_log emission on uploads.
	// Tests that assert audit rows pass the same pgxpool used by
	// Store; tests that don't care leave this nil.
	AuditExecer registry.AuditExecer
	// LFSMaxBytes overrides the default 200 GiB cap on PUT
	// /lfs-storage/{oid}.  Tests that exercise the size-cap
	// invariant set this to a small value so they can submit a
	// (cap+1)-byte payload without burning real disk.  Zero
	// uses the package default.
	LFSMaxBytes int64

	// CommitMaxBytes overrides the default 64 MiB cap on POST
	// /api/{type}/{repo}/commit/{rev}.  Tests that exercise
	// the allocator-amplification guard set this to a small
	// value (e.g. 4 KiB) so they can craft a body that crosses
	// the cap without keeping MiB of NDJSON in memory.
	CommitMaxBytes int64
}

// Stack is a running test stack: mock Hub + pulsys listening on
// loopback. t.Cleanup is registered for both.
type Stack struct {
	Mock     *mockhub.Server
	Proxy    *http.Server
	ProxyLn  net.Listener
	CacheDir string

	Cfg   *config.Config
	Store *cache.Store
	Core  *coreserver.Server
}

// New starts a Stack and registers cleanup. ProxyURL() returns
// http://127.0.0.1:PORT once this returns.
func New(t testing.TB, cfg Config) *Stack {
	t.Helper()
	if cfg.CacheDir == "" {
		cfg.CacheDir = t.TempDir()
	}
	if cfg.FsyncPolicy == "" {
		cfg.FsyncPolicy = "none"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "warn"
	}

	mock := mockhub.New(t, cfg.Mock)
	mockURL, err := url.Parse(mock.URL())
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	publicBase := "http://" + ln.Addr().String()
	allowHosts := append([]string{mockURL.Host}, cfg.ExtraAllowHosts...)

	flagSet := flag.NewFlagSet("testserver", flag.ContinueOnError)
	args := []string{
		"-listen", ln.Addr().String(),
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cfg.CacheDir,
		"-public-base-url", publicBase,
		"-default-upstream-host", mockURL.Host,
		"-upstream-scheme", mockURL.Scheme,
		"-allow-host", joinCSV(allowHosts),
		"-log-level", cfg.LogLevel,
		"-fsync", cfg.FsyncPolicy,
	}
	if cfg.Offline {
		args = append(args, "-strict-offline")
	}
	proxyCfg, err := config.ParseFlags(flagSet, args)
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	log := logx.New(cfg.LogLevel)
	telemetry.Register()
	store, err := cache.NewStore(cfg.CacheDir, cfg.FsyncPolicy)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	up := upstream.New(proxyCfg)
	inner := http.Handler(proxy.NewHandler(proxyCfg, store, up, log))

	var topHandler = inner
	if cfg.Registry != nil {
		topHandler = &proxy.RegistryHandler{
			Store:          cfg.Registry.Store,
			Blobs:          cfg.Registry.Blobs,
			TenantID:       cfg.Registry.TenantID,
			Next:           inner,
			PublicURL:      publicBase,
			AuditPool:      cfg.Registry.AuditExecer,
			LFSMaxBytes:    cfg.Registry.LFSMaxBytes,
			CommitMaxBytes: cfg.Registry.CommitMaxBytes,
		}
	}

	// Resolve io_uring preference: per-Stack opt-in wins; otherwise
	// the process-wide envIoUring switch.  See the env constant
	// comments for the docker-compose runbook this enables.
	wantIoUring := cfg.IoUring || envTruthy(envIoUring)
	if envTruthy(envIoUringRequire) && !wantIoUring {
		t.Fatalf("%s is set but io_uring was not enabled for this stack", envIoUringRequire)
	}

	core := &coreserver.Server{
		Cfg:               proxyCfg,
		Store:             store,
		Fallback:          coreserver.HandlerFallback(topHandler),
		AuthGate:          cfg.AuthGate,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       cfg.IdleTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		MaxConnsPerIP:     cfg.MaxConnsPerIP,
		SocketSendBuf:     proxyCfg.SocketSendBuf,
		TCPCork:           proxyCfg.TCPCork,
		IoUring:           wantIoUring,
	}

	stack := &Stack{
		Mock:     mock,
		ProxyLn:  ln,
		CacheDir: cfg.CacheDir,
		Cfg:      proxyCfg,
		Store:    store,
		Core:     core,
	}

	go func() { _ = core.Serve(ln) }()

	t.Cleanup(func() {
		core.Close()
		_ = ln.Close()
	})

	if err := waitForListen(publicBase + "/healthz"); err != nil {
		t.Fatalf("proxy did not come up: %v", err)
	}
	return stack
}

// ProxyURL returns the http://127.0.0.1:PORT base of the proxy. Plug
// into HF_ENDPOINT or use as the destination of HTTP test calls.
func (s *Stack) ProxyURL() string {
	return "http://" + s.ProxyLn.Addr().String()
}

// MockURL returns the upstream mock Hub URL. Most tests use
// ProxyURL() instead because the proxy will forward to MockURL.
func (s *Stack) MockURL() string {
	return s.Mock.URL()
}

// waitForListen polls a URL until it returns any HTTP response, up to
// 5 seconds. Used to avoid a race between Serve() and the first
// request from a test.
func waitForListen(probeURL string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s", probeURL)
		}
		req, _ := http.NewRequest(http.MethodGet, probeURL, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req = req.WithContext(ctx)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func joinCSV(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
