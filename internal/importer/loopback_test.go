// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package importer_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/importer"
	"github.com/pulsys-io/pulsys/internal/logx"
	"github.com/pulsys-io/pulsys/internal/upstream"
)

type loopbackXetUpstream struct {
	body []byte
	hash string
}

func (u *loopbackXetUpstream) Do(_ context.Context, _, host, path, _ string, _ http.Header, _ []byte) (*upstream.Response, error) {
	switch host {
	case "huggingface.co":
		if !strings.Contains(path, "/resolve/") {
			return nil, errors.New("unexpected hf path: " + path)
		}
		loc := fmt.Sprintf("https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/%s?sig=1", u.hash)
		h := http.Header{}
		h.Set("Location", loc)
		return &upstream.Response{
			Status: http.StatusFound,
			Header: h,
			Body:   io.NopCloser(bytes.NewReader(nil)),
		}, nil
	case "cas-bridge.xethub.hf.co":
		return &upstream.Response{
			Status:        http.StatusOK,
			Header:        http.Header{"Content-Length": []string{fmt.Sprint(len(u.body))}},
			ContentLength: int64(len(u.body)),
			Body:          io.NopCloser(bytes.NewReader(u.body)),
		}, nil
	}
	return nil, errors.New("unexpected host: " + host)
}

func TestNewLoopbackProxy_RedirectUsesLoopbackBase(t *testing.T) {
	fake := &loopbackXetUpstream{body: []byte("payload"), hash: "abc123"}
	dir := t.TempDir()
	cfg, err := config.ParseFlags(flag.NewFlagSet("loopback", flag.ContinueOnError), []string{
		"-listen", "127.0.0.1:0",
		"-admin-listen", "127.0.0.1:0",
		"-cache-dir", filepath.Join(dir, "cache"),
		"-public-base-url", "http://localhost:8082",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewStore(cfg.CacheDir, "none")
	if err != nil {
		t.Fatal(err)
	}
	loopback := importer.NewLoopbackProxy(cfg, store, fake, logx.New("error"))
	defer loopback.Close()

	resp, err := (&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Get(loopback.URL() + "/org/model/resolve/main/weights.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, loopback.URL()) {
		t.Fatalf("location=%q want prefix %q (must not use external public base)", loc, loopback.URL())
	}
	if strings.Contains(loc, "localhost:8082") {
		t.Fatalf("location leaked external public base: %q", loc)
	}
}
