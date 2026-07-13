// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/pulsys-io/pulsys/internal/db"
	"github.com/pulsys-io/pulsys/internal/telemetry"
)

// Registry holds Pulsys Prometheus collectors.
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry builds a dedicated registry (not the global default).
func NewRegistry() *Registry {
	r := prometheus.NewRegistry()
	registerProxyCounters(r)
	return &Registry{reg: r}
}

// Handler serves GET /metrics from the dedicated registry.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

func registerProxyCounters(reg *prometheus.Registry) {
	counters := []struct {
		name string
		help string
		fn   func() float64
	}{
		{"pulsys_cache_hits", "Warm cache hits served to clients", func() float64 { _, _, _, _, h, _ := telemetry.Snapshot(); return float64(h) }},
		{"pulsys_cache_misses", "Cache misses requiring upstream or offline refusal", func() float64 { _, _, _, _, _, m := telemetry.Snapshot(); return float64(m) }},
		{"pulsys_artifact_upstream_bytes", "Bytes fetched from upstream for artifact bodies", func() float64 { v, _, _, _, _, _ := telemetry.Snapshot(); return float64(v) }},
		{"pulsys_metadata_upstream_bytes", "Bytes fetched from upstream for metadata", func() float64 { _, v, _, _, _, _ := telemetry.Snapshot(); return float64(v) }},
		{"pulsys_artifact_upstream_fetches", "Upstream artifact fetch operations", func() float64 { _, _, v, _, _, _ := telemetry.Snapshot(); return float64(v) }},
		{"pulsys_metadata_upstream_fetches", "Upstream metadata fetch operations", func() float64 { _, _, _, v, _, _ := telemetry.Snapshot(); return float64(v) }},
		{"pulsys_sendfile_fused_calls", "Darwin sf_hdtr fused sendfile calls", func() float64 { f, _, _ := telemetry.SyscallSnapshot(); return float64(f) }},
		{"pulsys_sendfile_body_only_calls", "Plain sendfile body calls", func() float64 { _, b, _ := telemetry.SyscallSnapshot(); return float64(b) }},
		{"pulsys_io_uring_fused_calls", "Linux io_uring fused warm responses", func() float64 { return float64(telemetry.IoUringFusedSnapshot()) }},
		{"pulsys_offline_refusals", "Cache misses refused in offline mode", func() float64 { return float64(telemetry.OfflineRefusalsSnapshot()) }},
		{"pulsys_cache_quota_rejects", "Cache fill attempts rejected because the configured storage quota was full", func() float64 { return float64(telemetry.CacheQuotaRejectsSnapshot()) }},
	}
	for _, c := range counters {
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: c.name,
			Help: c.help,
		}, c.fn))
	}
}

// HealthHandler returns 200 when optional Postgres is reachable.
func HealthHandler(pool *db.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool != nil {
			hr := pool.Health(r.Context())
			if !hr.Healthy {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("unhealthy"))
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
