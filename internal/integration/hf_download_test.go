// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build integration

package integration

import (
	"bytes"
	"context"
	"flag"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/coreserver"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/proxy"
	"github.com/pulsys-io/pulsys/internal/telemetry"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

func TestHFCLIDownloadSmoke(t *testing.T) {
	if os.Getenv("HF_INTEGRATION") != "1" {
		t.Skip("set HF_INTEGRATION=1 (requires network + hf CLI)")
	}
	if _, err := exec.LookPath("hf"); err != nil {
		t.Skip("hf not on PATH")
	}

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	public := "http://" + addr
	args := []string{"-listen", addr, "-admin-listen", "127.0.0.1:0", "-cache-dir", cacheDir, "-public-base-url", public}
	cfg, err := config.ParseFlags(flag.NewFlagSet("x", flag.ContinueOnError), args)
	if err != nil {
		t.Fatal(err)
	}
	log := logx.New("warn")
	telemetry.Register()
	store, err := cache.NewStore(cacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	up := upstream.New(cfg)
	h := proxy.NewHandler(cfg, store, up, log)
	core := &coreserver.Server{
		Cfg:          cfg,
		Store:        store,
		Fallback:     coreserver.HandlerFallback(h),
		ReadTimeout:  15 * time.Minute,
		WriteTimeout: 15 * time.Minute,
	}
	go func() { _ = core.Serve(ln) }()
	defer func() { core.Close(); _ = ln.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "hf", "download", "hf-internal-testing/tiny-random-gpt2", "--local-dir", filepath.Join(dir, "model"))
	cmd.Env = append(os.Environ(), "HF_ENDPOINT="+public, "HF_HUB_DISABLE_TELEMETRY=1")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("hf download: %v\n%s", err, out.String())
	}
	var diskBytes int64
	_ = filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == "body" {
			diskBytes += info.Size()
		}
		return nil
	})
	if diskBytes == 0 {
		t.Fatalf("expected cached body bytes on disk under %s", cacheDir)
	}

	// Second download to a fresh local dir — should hit disk cache for artifact GETs.
	var out2 bytes.Buffer
	cmd2 := exec.CommandContext(ctx, "hf", "download", "hf-internal-testing/tiny-random-gpt2", "--local-dir", filepath.Join(dir, "model2"))
	cmd2.Env = append(os.Environ(), "HF_ENDPOINT="+public, "HF_HUB_DISABLE_TELEMETRY=1")
	cmd2.Stdout, cmd2.Stderr = &out2, &out2
	if err := cmd2.Run(); err != nil {
		t.Fatalf("second hf download: %v\n%s", err, out2.String())
	}
}
