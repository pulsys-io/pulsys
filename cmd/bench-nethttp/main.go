// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Command bench-nethttp serves a directory of static files via Go's
// stdlib net/http file server.  It exists as a same-machine, same-payload
// reference point alongside cmd/bench-coreserver so the README chart is
// honest and reproducible.
//
// Optionally mounts the directory under a URL prefix so that we can hit
// the same path shape Olah does (/models/<org>/<repo>/resolve/<commit>/<file>).
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:18383", "TCP listen address")
	root := flag.String("www", "tmp/bench/www", "directory to serve")
	prefix := flag.String("url-prefix", "/", "URL prefix to mount (e.g. /models/bench/bench/resolve/main/)")
	flag.Parse()

	p := *prefix
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	mux := http.NewServeMux()
	if p == "/" {
		mux.Handle("/", http.FileServer(http.Dir(*root)))
	} else {
		mux.Handle(p, http.StripPrefix(strings.TrimSuffix(p, "/"), http.FileServer(http.Dir(*root))))
	}

	log.Printf("bench-nethttp listening on %s, root=%s, prefix=%s", *addr, *root, p)
	srv := &http.Server{Addr: *addr, Handler: mux}
	log.Fatal(srv.ListenAndServe())
}
