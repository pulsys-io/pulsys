// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build linux

package coreserver_test

import (
	"bufio"
	"bytes"
	"flag"
	"net"
	"path/filepath"
	"testing"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
	"github.com/pulsys-io/pulsys/internal/telemetry"
)

func TestWarmHitIoUring(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://core.test",
		"-tcp-cork=false",
		"-iouring=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 4096)
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	path := "/models/bench/bench/resolve/main/4k.bin"
	keyHex := cache.KeyHex("GET", cfg.DefaultHost, path, "", "")
	if _, err := store.WriteFullFromStream(
		keyHex, 200, cfg.DefaultHost, path, "",
		`"etag"`, "application/octet-stream",
		bytes.NewReader(payload), int64(len(payload)),
	); err != nil {
		t.Fatal(err)
	}

	srv := &coreserver.Server{
		Cfg:     cfg,
		Store:   store,
		TCPCork: false,
		IoUring: true,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	var sink [8192]byte
	n := rawWarmGet(t, conn, br, "HTTP/1.1", cfg.DefaultHost, path, sink[:])
	if n < len(payload) {
		t.Fatalf("short read: got %d want >= %d", n, len(payload))
	}
	if telemetry.IoUringFusedSnapshot() < 1 {
		t.Skip("io_uring path not active on this kernel (fell back to sendfile)")
	}
}
