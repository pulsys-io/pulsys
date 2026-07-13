// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package telemetry

import (
	"expvar"
	"sync"
	"sync/atomic"
)

var (
	artifactUpstreamBytes   atomic.Int64
	metadataUpstreamBytes   atomic.Int64
	artifactUpstreamFetches atomic.Int64
	metadataUpstreamFetches atomic.Int64
	cacheHits               atomic.Int64
	cacheMisses             atomic.Int64
	clientBytesServed       atomic.Int64
	diskBytesWritten        atomic.Int64
	cacheCheckpoints        atomic.Int64
	cacheQuotaRejects       atomic.Int64

	// Syscall accounting on the warm-hit hot path.  These are the
	// "kernel-side floor" counters: the number of distinct sendfile(2)
	// invocations issued per warm response.
	//
	//   sendfileFusedCalls         (Darwin only) sf_hdtr fused calls;
	//                              each one ships HTTP head + body in
	//                              a SINGLE syscall.  In steady state
	//                              this counter == cacheHits, i.e. one
	//                              syscall per warm response.
	//
	//   sendfileBodyOnlyCalls      Plain sendfile(2) calls.  On Linux
	//                              every warm response increments this
	//                              once for the body (headers already
	//                              flushed via writev).  On Darwin this
	//                              is incremented only when sf_hdtr is
	//                              unavailable (e.g. the test in-memory
	//                              pipes) or when the kernel returned
	//                              EAGAIN partway through and we re-
	//                              entered with a header-less call.
	//
	//   sendfileEAGAINs            Number of times the kernel returned
	//                              EAGAIN and we parked on the netpoller
	//                              before re-issuing.  In steady state
	//                              on a fast loopback this stays at 0;
	//                              under heavy congestion it grows but
	//                              never exceeds (bytes / SOCK_BUFSIZE)
	//                              per response.
	sendfileFusedCalls    atomic.Int64
	sendfileBodyOnlyCalls atomic.Int64
	sendfileEAGAINs       atomic.Int64

	// tcpCorkCalls counts how many warm responses were served with
	// TCP_CORK turned on around the write(headers) + sendfile(body)
	// pair (Linux only; on Darwin sf_hdtr fuses the pair into one
	// syscall and cork is not used).  In steady state on Linux this
	// counter == cacheHits.  Used by the A6 bench harness to assert
	// the variant being tested actually exercised the cork path.
	tcpCorkCalls atomic.Int64

	// ioUringFusedCalls counts warm responses shipped via linked
	// io_uring WRITE+SPLICE (Linux Option B path).
	ioUringFusedCalls atomic.Int64

	// offlineRefusals counts how many cache misses were refused with
	// 504 because -strict-offline is set.  In a correctly-warmed cache against
	// a well-behaved client this is always 0.  Any non-zero value is a
	// signed contract violation: either a cache-key mismatch (bug) or
	// the client asked for something legitimately uncached (new file).
	offlineRefusals atomic.Int64

	// proxyPerIPCapDropped counts how many accepted TCP connections
	// were immediately closed because the accept loop's per-peer-IP
	// cap (Server.MaxConnsPerIP) was already at its limit.  Always
	// 0 when MaxConnsPerIP <= 0 (cap disabled).  A persistently
	// non-zero rate signals either a misconfigured cap or an
	// active abuser; the slowloris regression tests assert this
	// goes up when N+1 sockets are opened from one peer.
	proxyPerIPCapDropped atomic.Int64

	// inflightContendedPassthrough counts artifact GETs on the public
	// ingress that could not claim their in-flight byte range within
	// InflightAcquireTimeout and were served via an independent
	// (non-caching) pass-through fetch instead of blocking.  Normally 0;
	// a non-zero rate means concurrent fetches of the same uncached object
	// (e.g. an import running while a client downloads the same model).
	inflightContendedPassthrough atomic.Int64

	// Parser-error counters partitioned by error class.  These are
	// the primary monitoring signal for HTTP smuggling probe
	// campaigns: a spike in parserSmugglingSuspect or
	// parserBadRequest from a single peer IP is the textbook
	// fingerprint of a desync scanner (Burp Turbo Intruder,
	// http-desync-scanner, custom raw-TCP fuzz).
	//
	// In steady-state production these stay near zero because
	// well-behaved clients (hf-cli, browsers, real LBs) do not
	// generate malformed wire bytes.  Operators SHOULD alert when
	// the rate exceeds ~1/min/peer.
	//
	//   parserBadRequest         400-class shape (malformed line,
	//                            invalid header name, etc.).
	//                            Connection MAY be reused by the
	//                            server.
	//   parserSmugglingSuspect   400-class shape that matches a
	//                            known smuggling fingerprint
	//                            (duplicate CL, TE+CL, obs-fold,
	//                            CR/NUL in value).  Connection IS
	//                            closed.  Highest-severity signal.
	//   parserHeaderTooLarge     431-class shape (header block
	//                            exceeds the 16 KiB scratch cap).
	//                            Can be benign (large legitimate
	//                            cookies) or a memory-pressure
	//                            probe.
	parserBadRequest       atomic.Int64
	parserSmugglingSuspect atomic.Int64
	parserHeaderTooLarge   atomic.Int64
)

var registerOnce sync.Once

// Register publishes pulsys counters on the default expvar handler
// (/debug/vars).  Safe to call multiple times.
func Register() {
	registerOnce.Do(register)
}

func register() {
	expvar.Publish("pulsys_artifact_upstream_bytes", expvar.Func(func() any {
		return artifactUpstreamBytes.Load()
	}))
	expvar.Publish("pulsys_metadata_upstream_bytes", expvar.Func(func() any {
		return metadataUpstreamBytes.Load()
	}))
	expvar.Publish("pulsys_artifact_upstream_fetches", expvar.Func(func() any {
		return artifactUpstreamFetches.Load()
	}))
	expvar.Publish("pulsys_metadata_upstream_fetches", expvar.Func(func() any {
		return metadataUpstreamFetches.Load()
	}))
	expvar.Publish("pulsys_cache_hits", expvar.Func(func() any { return cacheHits.Load() }))
	expvar.Publish("pulsys_cache_misses", expvar.Func(func() any { return cacheMisses.Load() }))
	expvar.Publish("pulsys_client_bytes_served", expvar.Func(func() any {
		return clientBytesServed.Load()
	}))
	expvar.Publish("pulsys_disk_bytes_written", expvar.Func(func() any {
		return diskBytesWritten.Load()
	}))
	expvar.Publish("pulsys_cache_checkpoints", expvar.Func(func() any {
		return cacheCheckpoints.Load()
	}))
	expvar.Publish("pulsys_cache_quota_rejects", expvar.Func(func() any {
		return cacheQuotaRejects.Load()
	}))
	expvar.Publish("pulsys_sendfile_fused_calls", expvar.Func(func() any {
		return sendfileFusedCalls.Load()
	}))
	expvar.Publish("pulsys_sendfile_body_only_calls", expvar.Func(func() any {
		return sendfileBodyOnlyCalls.Load()
	}))
	expvar.Publish("pulsys_sendfile_eagains", expvar.Func(func() any {
		return sendfileEAGAINs.Load()
	}))
	expvar.Publish("pulsys_tcp_cork_calls", expvar.Func(func() any {
		return tcpCorkCalls.Load()
	}))
	expvar.Publish("pulsys_io_uring_fused_calls", expvar.Func(func() any {
		return ioUringFusedCalls.Load()
	}))
	expvar.Publish("pulsys_offline_refusals", expvar.Func(func() any {
		return offlineRefusals.Load()
	}))
	expvar.Publish("pulsys_proxy_per_ip_cap_dropped", expvar.Func(func() any {
		return proxyPerIPCapDropped.Load()
	}))
	expvar.Publish("pulsys_inflight_contended_passthrough", expvar.Func(func() any {
		return inflightContendedPassthrough.Load()
	}))
	expvar.Publish("pulsys_parser_errors", expvar.Func(func() any {
		return map[string]int64{
			"bad_request":       parserBadRequest.Load(),
			"smuggling_suspect": parserSmugglingSuspect.Load(),
			"header_too_large":  parserHeaderTooLarge.Load(),
		}
	}))
}

func AddArtifactUpstreamBytes(n int64) { artifactUpstreamBytes.Add(n) }
func AddMetadataUpstreamBytes(n int64) { metadataUpstreamBytes.Add(n) }
func IncArtifactUpstreamFetch()        { artifactUpstreamFetches.Add(1) }
func IncMetadataUpstreamFetch()        { metadataUpstreamFetches.Add(1) }
func IncCacheHit()                     { cacheHits.Add(1) }
func IncCacheMiss()                    { cacheMisses.Add(1) }
func AddClientBytesServed(n int64)     { clientBytesServed.Add(n) }
func AddDiskBytesWritten(n int64)      { diskBytesWritten.Add(n) }
func IncCacheCheckpoint()              { cacheCheckpoints.Add(1) }
func IncCacheQuotaReject()             { cacheQuotaRejects.Add(1) }
func IncSendfileFusedCall()            { sendfileFusedCalls.Add(1) }
func IncSendfileBodyOnlyCall()         { sendfileBodyOnlyCalls.Add(1) }
func IncSendfileEAGAIN()               { sendfileEAGAINs.Add(1) }
func IncTCPCorkCall()                  { tcpCorkCalls.Add(1) }
func IncIoUringFusedCall()             { ioUringFusedCalls.Add(1) }
func IncOfflineRefusals()              { offlineRefusals.Add(1) }
func IncProxyPerIPCapDropped()         { proxyPerIPCapDropped.Add(1) }
func IncInflightContendedPassthrough() { inflightContendedPassthrough.Add(1) }
func IncParserBadRequest()             { parserBadRequest.Add(1) }
func IncParserSmugglingSuspect()       { parserSmugglingSuspect.Add(1) }
func IncParserHeaderTooLarge()         { parserHeaderTooLarge.Add(1) }

// OfflineRefusalsSnapshot returns the offline-refusals counter for tests.
func OfflineRefusalsSnapshot() int64 { return offlineRefusals.Load() }

func CacheQuotaRejectsSnapshot() int64 { return cacheQuotaRejects.Load() }

// ProxyPerIPCapDroppedSnapshot returns the per-IP cap drop counter
// for tests/diagnostic harnesses (Phase 5 slowloris matrix).
func ProxyPerIPCapDroppedSnapshot() int64 { return proxyPerIPCapDropped.Load() }

// InflightContendedPassthroughSnapshot returns the contended-passthrough
// counter for tests/diagnostics.
func InflightContendedPassthroughSnapshot() int64 { return inflightContendedPassthrough.Load() }

// ParserErrorSnapshot returns the parser-error counters split
// by class.  Used by the CVE-monitoring regression tests to
// confirm a probe trips the right counter.
func ParserErrorSnapshot() (badRequest, smugglingSuspect, headerTooLarge int64) {
	return parserBadRequest.Load(), parserSmugglingSuspect.Load(), parserHeaderTooLarge.Load()
}

// SyscallSnapshot returns the current sendfile-syscall accounting for
// tests/diagnostic harnesses.
func SyscallSnapshot() (fused, bodyOnly, eagains int64) {
	return sendfileFusedCalls.Load(), sendfileBodyOnlyCalls.Load(), sendfileEAGAINs.Load()
}

// TCPCorkSnapshot returns the current TCP_CORK call counter for tests.
func TCPCorkSnapshot() int64 { return tcpCorkCalls.Load() }

// IoUringFusedSnapshot returns io_uring fused-response counter for tests.
func IoUringFusedSnapshot() int64 { return ioUringFusedCalls.Load() }

// Snapshot returns current counters (for tests).
func Snapshot() (artifactBytes, metadataBytes, artifactFetch, metadataFetch, hits, misses int64) {
	return artifactUpstreamBytes.Load(), metadataUpstreamBytes.Load(),
		artifactUpstreamFetches.Load(), metadataUpstreamFetches.Load(),
		cacheHits.Load(), cacheMisses.Load()
}
