// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Pprof / expvar listener -- ISOLATED.
//
// PURPOSE
//
//	This file is the ONLY place pulsys imports net/http/pprof and
//	expvar.  Both packages register handlers on http.DefaultServeMux
//	via blank-import side effects, which previously bled into every
//	handler that served http.DefaultServeMux directly or indirectly.
//	The 2026-05-21 incident showed why that's bad: /debug/pprof/*
//	and /debug/vars were reachable on the admin port without any
//	authentication, leaking heap, goroutine state, full process
//	environment, and the command line.
//
// DESIGN
//   - Blank imports live in this single file so the surface is
//     trivially auditable: `grep '_ "net/http/pprof"' cmd/` returns
//     exactly one match.
//   - startPprof builds a fresh *http.ServeMux and explicitly mounts
//     the well-known debug routes from http.DefaultServeMux.  This
//     gives us a dedicated handler we attach to a dedicated listener;
//     no other handler in the process can accidentally inherit the
//     debug routes.
//   - The listener is opt-in (cfg.PprofListen == "" disables) and
//     intended for loopback only.  We log a warning if the operator
//     binds anywhere other than localhost / 127.0.0.1 / ::1.
package main

import (
	"expvar"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"
)

func startPprof(addr string, log *slog.Logger) *http.Server {
	if !isLoopbackBind(addr) {
		log.Warn("pprof listener bound to non-loopback address; this exposes heap, goroutines, env, and command line. Use a loopback bind unless you understand the risk.",
			"addr", addr)
	}

	mux := http.NewServeMux()
	// Re-implement the routes net/http/pprof registers on
	// DefaultServeMux, against our own mux.  This is the same
	// list the upstream package's init() registers; see
	// https://pkg.go.dev/net/http/pprof.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// expvar registers /debug/vars on DefaultServeMux only; we
	// hand-wire it here so it shares the same listener.
	mux.Handle("/debug/vars", expvar.Handler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("pprof listener", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("pprof listener", "err", err)
		}
	}()
	return srv
}

// isLoopbackBind reports whether addr's host portion is a loopback
// literal (no DNS lookup; this is a quick safety hint only, not a
// definitive check).  An empty host (e.g. ":6061") binds to all
// interfaces and counts as non-loopback for our warning.
func isLoopbackBind(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]") // strip IPv6 brackets if any
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}
