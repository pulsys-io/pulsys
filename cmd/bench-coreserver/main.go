// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Command bench-coreserver starts pulsys's warm-hit fast path
// (internal/coreserver) backed by a pre-populated cache, on a fixed
// loopback port.  It is used for apples-to-apples benchmarks against
// nginx, caddy, and Go's net/http file server (see
// scripts/bench_compare.sh).
//
// This binary is NOT for production use.  It exists only so that wrk
// (or any HTTP/1.1 load generator) can exercise the same code path
// that serves warm cache hits in the real proxy.
package main

import (
	"bytes"
	"flag"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:18080", "TCP listen address")
	cacheDir := flag.String("cache-dir", "", "cache directory (created if empty)")
	wwwDir := flag.String("www", "tmp/bench/www", "directory of payload files to seed the cache with (each file becomes /<name>)")
	host := flag.String("host", "huggingface.co", "Host header to seed the cache key with")
	urlPrefix := flag.String("url-prefix", "/", "URL prefix to seed each payload under (e.g. /models/bench/bench/resolve/main/ for HF-shaped URLs)")
	pprofAddr := flag.String("pprof", "", "optional pprof http listen address (e.g. 127.0.0.1:18081); empty to disable")
	flag.Parse()

	if *pprofAddr != "" {
		go func() {
			log.Printf("bench-coreserver pprof on http://%s/debug/pprof/", *pprofAddr)
			log.Println(http.ListenAndServe(*pprofAddr, nil))
		}()
	}

	cd := *cacheDir
	if cd == "" {
		var err error
		cd, err = os.MkdirTemp("", "bench-coreserver-cache-")
		if err != nil {
			log.Fatalf("temp cache: %v", err)
		}
		defer func() { _ = os.RemoveAll(cd) }()
	}
	cfg, err := config.ParseFlags(flag.NewFlagSet("bench", flag.ContinueOnError), []string{
		"-listen", *addr,
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", cd,
		"-public-base-url", "http://" + *addr,
		"-default-upstream-host", *host,
	})
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		log.Fatalf("cache: %v", err)
	}

	entries, err := os.ReadDir(*wwwDir)
	if err != nil {
		log.Fatalf("read www: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(*wwwDir, e.Name())
		body, err := os.ReadFile(full)
		if err != nil {
			log.Fatalf("read %s: %v", full, err)
		}
		prefix := *urlPrefix
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		urlPath := prefix + e.Name()
		keyHex := cache.KeyHex("GET", cfg.DefaultHost, urlPath, "", "")
		if _, err := store.WriteFullFromStream(
			keyHex, 200, cfg.DefaultHost, urlPath, "",
			`"bench-`+e.Name()+`"`, "application/octet-stream",
			bytes.NewReader(body), int64(len(body)),
		); err != nil {
			log.Fatalf("seed %s: %v", urlPath, err)
		}
		log.Printf("seeded %s (%d bytes)", urlPath, len(body))
	}

	srv := &coreserver.Server{Cfg: cfg, Store: store}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("bench-coreserver listening on %s, cache=%s", ln.Addr(), cd)
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
