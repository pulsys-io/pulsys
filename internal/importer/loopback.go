// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer

import (
	"log/slog"
	"net/http/httptest"
	"net/url"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

// LoopbackProxy serves proxy.Handler on loopback without the data-plane PAT gate.
type LoopbackProxy struct {
	Server *httptest.Server
}

// URL returns the loopback base URL (no trailing slash).
func (l *LoopbackProxy) URL() string {
	if l == nil || l.Server == nil {
		return ""
	}
	return l.Server.URL
}

// Close stops the loopback server.
func (l *LoopbackProxy) Close() {
	if l != nil && l.Server != nil {
		l.Server.Close()
	}
}

// NewLoopbackProxy binds a dedicated proxy.Handler on 127.0.0.1:0 for in-process
// cache warm. PublicBaseURL is pinned to the listener address so Xet /_p/
// redirect targets stay on this server instead of the external ingress URL
// (e.g. http://localhost:8082 from compose, which is unreachable in-container).
func NewLoopbackProxy(baseCfg *config.Config, store *cache.Store, up upstream.Client, log *slog.Logger) *LoopbackProxy {
	srv := httptest.NewUnstartedServer(nil)
	loopbackURL, err := url.Parse("http://" + srv.Listener.Addr().String())
	if err != nil {
		panic("importer: loopback url: " + err.Error())
	}
	importCfg := *baseCfg
	importCfg.PublicBaseURL = loopbackURL
	// The importer is a cache-population job with no client-side header
	// timeout: it must WAIT for an overlapping in-flight fetch to populate
	// the cache rather than fall through to a non-caching pass-through (the
	// public-ingress behaviour).  Force the unbounded acquire path.
	importCfg.InflightAcquireTimeout = 0
	h := proxy.NewHandler(&importCfg, store, up, log)
	srv.Config.Handler = h
	srv.Start()
	return &LoopbackProxy{Server: srv}
}
