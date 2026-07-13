// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds process-wide settings for pulsys.
type Config struct {
	Listen      string
	AdminListen string
	// PprofListen, when non-empty, binds a SEPARATE HTTP listener
	// (typically loopback, e.g. 127.0.0.1:6061) and registers
	// net/http/pprof + expvar on it.  Default empty -> pprof is
	// NOT exposed.  This replaces the previous behavior of
	// mounting net/http/pprof and expvar on the admin port via
	// blank-import side effects, which surfaced /debug/pprof/*
	// and /debug/vars on every deployment regardless of opt-in.
	// See docs/security.md.
	PprofListen    string
	CacheDir       string
	DefaultHost    string // e.g. huggingface.co (host only, optional :port)
	UpstreamScheme string // "https" (default, production) or "http" (bench/test against fake-hf)
	PublicBaseURL  *url.URL
	AllowHost      []string // suffix or exact match entries (lowercase)
	LogLevel       string
	ReadTimeout    time.Duration
	// WriteTimeout bounds response writes.  It is NOT an absolute cap on
	// total transfer time: for large streamed bodies the coreserver
	// treats it as an IDLE deadline that slides forward as the client
	// keeps draining bytes (cold/fallback path: sliding SetWriteDeadline;
	// classic warm path: a progress-driven deadline pump; io_uring
	// reactor path: SO_SNDTIMEO).  So a multi-GiB artifact download over
	// a slow link completes as long as it keeps making progress, while a
	// client that stalls for WriteTimeout is still cut.  Zero uses the
	// coreserver package default.
	WriteTimeout time.Duration
	// InflightAcquireTimeout bounds how long an artifact GET on the
	// public ingress waits to claim its byte range behind another
	// in-flight fetch of the same object before falling through to an
	// independent (non-caching) pass-through fetch.  This prevents a long
	// whole-file fetch (a concurrent import, or another client's no-Range
	// GET) from blocking the request past the client's response-header
	// read timeout.  Keep it below that client timeout.  Zero uses the
	// default; the importer's loopback handler ignores this and always
	// waits unbounded (it has no client timeout and wants the cache warm).
	InflightAcquireTimeout time.Duration
	// IdleTimeout bounds the wait for the FIRST byte of a request
	// on a freshly accepted OR keep-alive-idle connection.  Zero
	// uses the coreserver package default (60s).  Slowloris defense.
	IdleTimeout time.Duration
	// ReadHeaderTimeout bounds the time between the first byte
	// of a request arriving and the headers being parsed.  Zero
	// uses the coreserver package default (5s).  Slowloris defense.
	ReadHeaderTimeout time.Duration
	// MaxConnsPerIP, when > 0, caps simultaneous accepted
	// connections per peer IP.  Zero (default) disables the cap;
	// production-behind-LB deployments usually leave this off and
	// let the LB do connection-flood defense.
	MaxConnsPerIP int
	MaxIdle       time.Duration
	TLSCertFile   string
	TLSKeyFile    string
	FsyncPolicy   string // none, per-file, dir
	MaxRedirects  int
	SocketSendBuf int  // SO_SNDBUF for accepted TCP conns; 0 = 4 MiB default, -1 = leave kernel default
	TCPCork       bool // Linux only: wrap write(headers)+sendfile(body) in TCP_CORK to coalesce them into one segment.  No-op on Darwin (sf_hdtr already fuses).
	IoUring       bool // Linux only: use io_uring linked WRITE+SPLICE SQEs instead of cork+sendfile.  Requires kernel >= 6.1; falls back transparently if unavailable.
	Listeners     int  // Number of SO_REUSEPORT listeners on -listen (1 = single listener, no REUSEPORT).  Set to GOMAXPROCS or NumCPU for high-RPS workloads on multi-core hosts.
	// Offline enables serving cached metadata (tree, revision, etc.) on
	// GET/HEAD when present.  Artifact bodies are always cache-first.
	// When false, metadata always refreshes upstream for repo freshness.
	Offline bool

	// StrictOffline rejects upstream on cache miss with 504 (cached-only /
	// bench mode).  When false, cache misses pass through to upstream.
	StrictOffline bool

	// CacheMaxBytes caps total on-disk cache bytes.  Zero leaves
	// the cache unbounded (legacy behavior).  Positive values make
	// the cache enforce a hard quota: new cache fills are rejected
	// with 507 / ErrQuotaExceeded until an operator purges models
	// or raises the limit.  This is deliberately not an automatic
	// eviction policy; no background LRU deletes files.
	CacheMaxBytes int64

	// LFSMaxBytes bounds the body size accepted by PUT
	// /lfs-storage/{oid} (the registry upload endpoint).  Zero
	// uses the proxy.RegistryHandler default (200 GiB).
	// Enterprises shipping multi-100 GiB single-shard
	// checkpoints can raise this; the cap exists to prevent a
	// single misbehaving client from filling the proxy's disk
	// in one PUT.  Phase 5 WSTG-BUSL-09.
	LFSMaxBytes int64

	// CommitMaxBytes bounds the NDJSON body size accepted by
	// POST /api/{type}/{repo}/commit/{rev}.  Zero uses the
	// proxy.RegistryHandler default (64 MiB).  Bounds allocator
	// amplification on the commit JSON parser; legitimate
	// commits are << 1 MiB because binary content goes via
	// LFS PUT.  Modeled on CVE-2025-58185 (encoding/asn1)
	// and the HTTP/2 CONTINUATION-flood class.
	CommitMaxBytes int64

	// UpstreamHFToken is Pulsys's own read-only Hugging Face token,
	// read from the PULSYS_HF_TOKEN environment variable.  Pulsys
	// authenticates to Hugging Face with THIS token on every cold-miss
	// read and import, and never forwards the caller's inbound
	// Authorization upstream: clients authenticate to Pulsys (with a
	// pulsys_* PAT), and Pulsys authenticates to Hugging Face.  The
	// pulsys binary requires it at startup (cmd/pulsys/main.go); it is
	// intentionally env-only, never a flag, so it does not appear in the
	// process command line.  ParseFlags does not enforce its presence so
	// that proxy.Handler unit tests can construct a Config without it.
	UpstreamHFToken string
}

func defaultAllowHosts() []string {
	return []string{
		"huggingface.co",
		"hf.co",
		"huggingface.com",
		"cdn-lfs.huggingface.co",
		"lfs.huggingface.co",
		"cas-bridge.xethub.hf.co",
		"xethub.hf.co",
	}
}

// ParseFlags parses command-line flags into Config.
func ParseFlags(fs *flag.FlagSet, args []string) (*Config, error) {
	cfg := &Config{
		ReadTimeout:            300 * time.Second,
		WriteTimeout:           300 * time.Second,
		InflightAcquireTimeout: 5 * time.Second,
		MaxIdle:                90 * time.Second,
		MaxRedirects:           10,
		FsyncPolicy:            "none",
		UpstreamScheme:         "https",
		TCPCork:                true,
		Listeners:              1,
		Offline:                true,
	}
	var allowList string
	var publicBase string

	fs.StringVar(&cfg.Listen, "listen", ":8080", "TCP address for the proxy ingress")
	fs.StringVar(&cfg.AdminListen, "admin-listen", "127.0.0.1:6060", "loopback address for the Pulsys admin HTTP surface (/auth/*, /admin/api/v1/*, /healthz, /metrics)")
	fs.StringVar(&cfg.PprofListen, "pprof-listen", "", "OPT-IN loopback address for net/http/pprof + expvar (e.g. 127.0.0.1:6061); empty disables pprof entirely. Never expose this to the public internet.")
	fs.StringVar(&cfg.CacheDir, "cache-dir", "./pulsys-cache", "root directory for disk cache")
	fs.StringVar(&cfg.DefaultHost, "default-upstream-host", "huggingface.co", "default upstream host (optionally host:port; omit port to use scheme default)")
	fs.StringVar(&cfg.UpstreamScheme, "upstream-scheme", cfg.UpstreamScheme, "upstream scheme: https (production) or http (bench/test against a local fake)")
	fs.StringVar(&publicBase, "public-base-url", "", "base URL for rewriting Location headers (e.g. http://127.0.0.1:8080); required for multi-host redirects")
	fs.StringVar(&allowList, "allow-host", strings.Join(defaultAllowHosts(), ","), "comma-separated allowlist of upstream host suffixes for /_p/ and redirects")
	fs.StringVar(&cfg.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.DurationVar(&cfg.ReadTimeout, "read-timeout", cfg.ReadTimeout, "upstream + request-body read timeout (default 300s; bounds the wall-clock budget for completing a request body once headers are parsed)")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "upstream + response write timeout (default 300s; for large streamed bodies it acts as an IDLE deadline that slides while the client keeps draining, so slow but progressing multi-GiB downloads are not truncated; a client stalled for this long is cut)")
	fs.DurationVar(&cfg.InflightAcquireTimeout, "inflight-acquire-timeout", cfg.InflightAcquireTimeout, "how long an artifact GET on the public ingress waits to claim its byte range behind another in-flight fetch of the same file before falling through to an independent pass-through fetch (default 5s; keep below the client's response-header read timeout so a concurrent whole-file download/import never makes the client time out). The importer's loopback path always waits unbounded.")
	fs.Int64Var(&cfg.CommitMaxBytes, "commit-max-bytes", 0, "max NDJSON body size accepted by POST /api/{type}/{repo}/commit/{rev}; 0 uses the 64 MiB default.  Bounds allocator amplification on registry commit; legitimate commits are far smaller (binaries go via LFS PUT).")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 0, "keep-alive idle timeout: how long an established connection may wait between requests before close (0 uses 60s default).  Phase 5 slowloris defense.")
	fs.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", 0, "header-arrival timeout: how long after the first byte of a request the headers must be fully parsed (0 uses 5s default).  Phase 5 slowloris defense.")
	fs.IntVar(&cfg.MaxConnsPerIP, "max-conns-per-ip", 0, "cap on simultaneous accepted connections from any single peer IP (0 disables the cap; the LB is the primary line of defense).  Phase 5 connection-flood guard.")
	fs.DurationVar(&cfg.MaxIdle, "max-idle-conn-timeout", cfg.MaxIdle, "upstream idle connection timeout")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", "", "optional TLS certificate for ingress (PEM)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", "", "optional TLS key for ingress (PEM)")
	fs.StringVar(&cfg.FsyncPolicy, "fsync", cfg.FsyncPolicy, "disk durability: none, per-file, dir")
	fs.IntVar(&cfg.MaxRedirects, "max-redirects", cfg.MaxRedirects, "max internal upstream redirects (prefer returning to client)")
	fs.IntVar(&cfg.SocketSendBuf, "socket-send-buf", 0, "SO_SNDBUF (bytes) for accepted TCP conns; 0=4MiB default, -1=leave kernel default (reduces sendfile syscalls on large bodies)")
	fs.BoolVar(&cfg.TCPCork, "tcp-cork", cfg.TCPCork, "Linux only: wrap write(headers)+sendfile(body) in TCP_CORK to coalesce them into one TCP segment; no-op on Darwin (sf_hdtr fuses regardless)")
	fs.BoolVar(&cfg.IoUring, "iouring", false, "Linux only (experimental): use io_uring linked WRITE+SPLICE SQEs instead of cork+sendfile; requires kernel >= 6.1; falls back transparently if unavailable")
	fs.IntVar(&cfg.Listeners, "listeners", cfg.Listeners, "number of SO_REUSEPORT listeners on -listen (1 disables REUSEPORT); set to GOMAXPROCS for high-RPS workloads on multi-core hosts")
	fs.BoolVar(&cfg.Offline, "offline", true, "serve cached metadata on GET/HEAD when present (default on). When false, metadata always refreshes upstream; artifact bodies remain cache-first")
	fs.BoolVar(&cfg.StrictOffline, "strict-offline", false, "on cache miss return 504 instead of fetching upstream (cached-only / bench mode; requires a fully warmed cache)")
	fs.Int64Var(&cfg.CacheMaxBytes, "cache-max-bytes", 0, "maximum total on-disk cache bytes; 0 leaves the cache unbounded. Positive values enforce a hard quota and reject new cache fills with 507 until models are purged or the limit is raised.")
	fs.Int64Var(&cfg.LFSMaxBytes, "lfs-max-bytes", 0, "maximum body size (bytes) accepted by PUT /lfs-storage/{oid}; 0 uses the package default (200 GiB).  Phase 5 WSTG-BUSL-09 disk-DoS guard.")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "pulsys: Hugging Face disk-caching reverse proxy (coreserver + net/http).\n\n")
		_, _ = fmt.Fprintf(fs.Output(), "Pulsys admin port: %s (auth + /admin/api/v1/* + /healthz + /metrics)\n", cfg.AdminListen)
		_, _ = fmt.Fprintf(fs.Output(), "Profiling: pass -pprof-listen 127.0.0.1:6061 (default: disabled).\n")
		_, _ = fmt.Fprintf(fs.Output(), "    go tool pprof http://127.0.0.1:6061/debug/pprof/profile?seconds=30\n\n")
		fs.PrintDefaults()
		_, _ = fmt.Fprintf(fs.Output(), "\nExample: HF_ENDPOINT=http://127.0.0.1:8080 hf download <repo>\n")
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if publicBase == "" {
		return nil, errors.New("-public-base-url is required (e.g. http://127.0.0.1:8080)")
	}
	u, err := url.Parse(publicBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid -public-base-url: %q", publicBase)
	}
	cfg.PublicBaseURL = u
	cfg.DefaultHost = strings.ToLower(strings.TrimSpace(cfg.DefaultHost))
	for _, p := range strings.Split(allowList, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			cfg.AllowHost = append(cfg.AllowHost, p)
		}
	}
	if len(cfg.AllowHost) == 0 {
		return nil, errors.New("allow-host list is empty")
	}
	switch cfg.FsyncPolicy {
	case "none", "per-file", "dir":
	default:
		return nil, fmt.Errorf("invalid -fsync: %q", cfg.FsyncPolicy)
	}
	switch cfg.UpstreamScheme {
	case "https", "http":
	default:
		return nil, fmt.Errorf("invalid -upstream-scheme: %q (want https|http)", cfg.UpstreamScheme)
	}
	// Read-only upstream credential is env-only (never a flag) so it is
	// not visible in the process command line.
	cfg.UpstreamHFToken = strings.TrimSpace(os.Getenv("PULSYS_HF_TOKEN"))
	return cfg, nil
}
