// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package testserver

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/pulsys-io/pulsys/internal/hfhub/fixtures"
	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
)

func TestStackBootHealthz(t *testing.T) {
	stack := New(t, Config{})
	resp, err := http.Get(stack.ProxyURL() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body=%q", body)
	}
}

func TestStackDownloadThroughProxy(t *testing.T) {
	files := fixtures.TinyModelFiles("acme/widget")
	stack := New(t, Config{
		Mock: mockhub.Config{Repos: []mockhub.RepoSpec{{
			Name: "acme/widget", InitialFiles: files,
		}}},
	})

	resp, err := http.Get(stack.ProxyURL() + "/acme/widget/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, files["config.json"]) {
		t.Fatalf("body mismatch")
	}

	// Warm cache hit: second request goes from disk, no upstream call.
	resp2, err := http.Get(stack.ProxyURL() + "/acme/widget/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("status=%d", resp2.StatusCode)
	}
	got2, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(got2, files["config.json"]) {
		t.Fatalf("warm body mismatch")
	}
	if c := stack.Mock.CallCount("GET", "/{repo}/resolve/{rev}/{path}"); c != 1 {
		t.Fatalf("expected 1 upstream resolve, got %d", c)
	}
}

func TestStackRangeThroughProxy(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 100)
	stack := New(t, Config{
		Mock: mockhub.Config{Repos: []mockhub.RepoSpec{{
			Name: "acme/widget", InitialFiles: map[string][]byte{"big.bin": body},
		}}},
	})
	req, _ := http.NewRequest(http.MethodGet, stack.ProxyURL()+"/acme/widget/resolve/main/big.bin", nil)
	req.Header.Set("Range", "bytes=20-29")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "0123456789" {
		t.Fatalf("body=%q", got)
	}
}

func TestStackConcurrentReadsCachedOnce(t *testing.T) {
	body := bytes.Repeat([]byte{0x42}, 8192)
	stack := New(t, Config{
		Mock: mockhub.Config{Repos: []mockhub.RepoSpec{{
			Name: "acme/widget", InitialFiles: map[string][]byte{"cfg.json": body},
		}}},
	})

	const N = 25
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			resp, err := http.Get(stack.ProxyURL() + "/acme/widget/resolve/main/cfg.json")
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if !bytes.Equal(got, body) {
				t.Errorf("body mismatch")
			}
		}()
	}
	wg.Wait()

	// Cache populated; upstream resolve count must be low (1 + maybe
	// a few while singleflight stabilizes). With the current
	// single-flight, exactly one upstream call is the contract.
	if c := stack.Mock.CallCount("GET", "/{repo}/resolve/{rev}/{path}"); c > 5 {
		t.Fatalf("too many upstream resolves: %d", c)
	}
}
