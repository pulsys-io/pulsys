// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package upstream

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/config"
)

// TestDo_RejectsDisallowedHost is the sink-level SSRF regression: the
// upstream client must refuse to issue a request to a host that is
// neither the configured default upstream nor on the allowlist, even
// if a caller passes one (CWE-918 / go/request-forgery).
func TestDo_RejectsDisallowedHost(t *testing.T) {
	// A stand-in upstream so an accidental request would actually
	// connect somewhere local instead of the real internet.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	srvHost := mustHost(t, srv.URL)

	cfg := parseCfg(t,
		"-default-upstream-host", srvHost,
		"-upstream-scheme", "http",
		"-public-base-url", "http://"+srvHost,
	)
	c := New(cfg)

	deny := []string{
		"127.0.0.1",
		"169.254.169.254",
		"metadata.google.internal",
		"10.0.0.1",
		"192.168.1.1",
		"evil.com",
		"huggingface.co.evil.com",
	}
	for _, host := range deny {
		resp, err := c.Do(context.Background(), http.MethodGet, host, "/x", "", nil, nil)
		if err == nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			t.Errorf("Do(host=%q) succeeded; want rejection", host)
			continue
		}
		if !strings.Contains(err.Error(), "host not allowed") {
			t.Errorf("Do(host=%q) error = %v; want 'host not allowed'", host, err)
		}
	}
}

// TestDo_AllowsDefaultAndAllowlistedHost confirms the gate does not
// break the legitimate paths: the configured default upstream (a
// loopback fake here) and a non-default allowlisted host.
func TestDo_AllowsDefaultHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	srvHost := mustHost(t, srv.URL)

	cfg := parseCfg(t,
		"-default-upstream-host", srvHost,
		"-upstream-scheme", "http",
		"-public-base-url", "http://"+srvHost,
	)
	c := New(cfg)

	resp, err := c.Do(context.Background(), http.MethodGet, srvHost, "/x", "", nil, nil)
	if err != nil {
		t.Fatalf("Do(default host) failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.Status != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.Status, http.StatusNoContent)
	}
}

func parseCfg(t *testing.T, args ...string) *config.Config {
	t.Helper()
	cfg, err := config.ParseFlags(flag.NewFlagSet("upstream-test", flag.ContinueOnError), args)
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return cfg
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}
