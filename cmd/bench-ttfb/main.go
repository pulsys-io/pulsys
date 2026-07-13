// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Command bench-ttfb measures keep-alive HTTP/1.1 request->first-byte
// latency (TTFB) against a single URL.
//
// Why TTFB (and not aggregate throughput)?
//
//	For a sidecar HF cache, the metric users actually feel is "how
//	long after my client called .download() does the first byte
//	arrive."  At small/mid payload sizes (configs, tokenisers,
//	individual safetensors shards), TTFB is the user-visible cost
//	-- the body transfer itself runs at the local sendfile(2) /
//	loopback ceiling once it starts, which is identical across any
//	competent Go server (see scripts/bench_compare.sh for proof).
//
//	By contrast, raw wrk throughput at >= 10 MiB payloads on
//	macOS sits at the kernel sendfile loopback ceiling (~5 GB/s
//	single-stream, ~8.5 GB/s aggregate); pulsys, Caddy, and
//	net/http all tie within ~5% run-to-run noise, which means
//	the metric measures the kernel, not the server.
//
// Methodology:
//
//   - Open --concurrency persistent keep-alive TCP connections.
//   - Per connection, run --requests serial round-trips: write
//     pre-baked HTTP/1.1 GET request, time monotonic-now to the
//     arrival of the first body byte (after the "\r\n\r\n"), then
//     drain the rest of the response silently.
//   - Aggregate one histogram across all conns.
//   - Print p50 / p90 / p99 / p99.9 / max in microseconds, plus a
//     one-line CSV row suitable for the bench renderer.
//
// We deliberately exclude:
//
//   - TCP connect time -- in production HF clients keep connections
//     warm; first-byte cost on a warm conn is what matters.
//   - DNS lookup (we accept --url with a literal IP:port).
//   - TLS handshake (sidecar is plain HTTP over loopback).
//
// This isolates the userspace serve cost: HTTP parse + dispatch
// + the very first sendfile(2) syscall.  Differences here are real
// and not masked by loopback bandwidth.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	target := flag.String("url", "http://127.0.0.1:18080/models/bench/bench/resolve/main/256k.bin", "URL to hit (scheme http, literal host:port)")
	concurrency := flag.Int("concurrency", 1, "number of persistent keep-alive connections")
	requests := flag.Int("requests", 2000, "requests per connection")
	warmup := flag.Int("warmup", 200, "warm-up requests per connection (excluded from stats)")
	label := flag.String("label", "", "label to emit in the trailing CSV row (e.g. pulsys)")
	csv := flag.String("csv", "", "append a one-line CSV row to this path: label,url,concurrency,requests,p50_us,p90_us,p99_us,p999_us,max_us")
	flag.Parse()

	u, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("bad --url: %v", err)
	}
	if u.Scheme != "http" {
		log.Fatalf("--url must be http (TLS not supported by this microbench)")
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	// Pre-bake the HTTP/1.1 GET so the inner loop only writes a fixed
	// byte slice and reads the response.  Identical across all conns;
	// any per-request work happens entirely on the server.
	reqBytes := []byte(fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: bench-ttfb/1\r\nAccept: */*\r\n\r\n", path, u.Host))

	type sample struct{ us int64 }
	perConn := make([][]sample, *concurrency)
	var wg sync.WaitGroup
	errs := make(chan error, *concurrency)

	t0 := time.Now()
	for i := 0; i < *concurrency; i++ {
		i := i
		perConn[i] = make([]sample, 0, *requests)
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", host)
			if err != nil {
				errs <- fmt.Errorf("conn %d dial: %w", i, err)
				return
			}
			defer func() { _ = conn.Close() }()
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			// 1 MiB read buffer -- big enough to slurp small bodies in a
			// single read after first-byte arrival, so subsequent rounds
			// don't fight with leftover bytes.
			br := bufio.NewReaderSize(conn, 1<<20)

			doOne := func() (time.Duration, error) {
				if _, err := conn.Write(reqBytes); err != nil {
					return 0, err
				}
				start := time.Now()
				// Read status line + headers (up to and including the
				// blank line); the very next byte is the first body
				// byte and what we time to.
				var (
					contentLength int64 = -1
					chunked       bool
				)
				for {
					line, err := br.ReadSlice('\n')
					if err != nil {
						return 0, err
					}
					if len(line) == 2 && line[0] == '\r' {
						break
					}
					// Cheap, case-insensitive header sniffing for two
					// fields we need to drain the body.  Avoids the
					// allocation of net/http.ReadResponse.
					if l := len(line); l > 16 && (line[0] == 'C' || line[0] == 'c') {
						switch {
						case asciiHasPrefixFold(line, "Content-Length:"):
							v := strings.TrimSpace(string(line[len("Content-Length:"):]))
							_, _ = fmt.Sscanf(v, "%d", &contentLength)
						case asciiHasPrefixFold(line, "Transfer-Encoding:"):
							v := strings.TrimSpace(string(line[len("Transfer-Encoding:"):]))
							if strings.EqualFold(v, "chunked") {
								chunked = true
							}
						}
					}
				}
				// Time to first body byte: peek 1 byte from the body.
				if _, err := br.Peek(1); err != nil {
					return 0, err
				}
				ttfb := time.Since(start)
				// Drain the rest of the response so the connection is
				// ready for the next request.
				if chunked {
					if err := drainChunked(br); err != nil {
						return ttfb, err
					}
				} else if contentLength > 0 {
					if _, err := io.CopyN(io.Discard, br, contentLength); err != nil {
						return ttfb, err
					}
				} else if contentLength < 0 {
					return ttfb, fmt.Errorf("unknown framing (no Content-Length, no chunked)")
				}
				return ttfb, nil
			}

			for j := 0; j < *warmup; j++ {
				if _, err := doOne(); err != nil {
					errs <- fmt.Errorf("conn %d warmup %d: %w", i, j, err)
					return
				}
			}
			for j := 0; j < *requests; j++ {
				d, err := doOne()
				if err != nil {
					errs <- fmt.Errorf("conn %d req %d: %w", i, j, err)
					return
				}
				perConn[i] = append(perConn[i], sample{us: d.Microseconds()})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		log.Fatal(e)
	}

	// Flatten and sort.
	all := make([]int64, 0, *concurrency*(*requests))
	for _, s := range perConn {
		for _, v := range s {
			all = append(all, v.us)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	if len(all) == 0 {
		log.Fatalf("no samples collected (every conn failed)")
	}

	p := func(q float64) int64 {
		idx := int(float64(len(all)-1) * q)
		return all[idx]
	}
	var sum int64
	for _, v := range all {
		sum += v
	}
	mean := float64(sum) / float64(len(all))

	dt := time.Since(t0)
	fmt.Printf("ttfb  url=%s  conns=%d  reqs/conn=%d  samples=%d  wall=%s\n", *target, *concurrency, *requests, len(all), dt.Truncate(time.Millisecond))
	fmt.Printf("ttfb  p50=%dus  p90=%dus  p99=%dus  p99.9=%dus  max=%dus  mean=%.1fus  min=%dus\n",
		p(0.50), p(0.90), p(0.99), p(0.999), all[len(all)-1], mean, all[0])

	if *csv != "" && *label != "" {
		f, err := os.OpenFile(*csv, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("csv: %v", err)
		}
		defer func() { _ = f.Close() }()
		_, _ = fmt.Fprintf(f, "%s,%s,%d,%d,%d,%d,%d,%d,%d\n",
			*label, *target, *concurrency, *requests,
			p(0.50), p(0.90), p(0.99), p(0.999), all[len(all)-1])
	}

	// Guard against the optimiser eliding work.
	_ = runtime.NumGoroutine()
	_ = rand.Intn
}

// asciiHasPrefixFold returns true if the ASCII-only header line starts
// with prefix, case-insensitively.  Hot enough on the inner loop to
// avoid strings.EqualFold + slicing.
func asciiHasPrefixFold(line []byte, prefix string) bool {
	if len(line) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a := line[i]
		b := prefix[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// drainChunked discards a Transfer-Encoding: chunked body, including
// the terminating "0\r\n\r\n".  Used only for servers that return
// chunked responses (Caddy occasionally does for HEAD-with-body
// quirks; pulsys does not).
func drainChunked(br *bufio.Reader) error {
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return err
		}
		// Strip CRLF and any chunk-extension after ";".
		s := strings.TrimRight(string(line), "\r\n")
		if i := strings.IndexByte(s, ';'); i >= 0 {
			s = s[:i]
		}
		var size int64
		if _, err := fmt.Sscanf(s, "%x", &size); err != nil {
			return fmt.Errorf("bad chunk size %q: %w", s, err)
		}
		if size == 0 {
			// Trailing CRLF after the zero-size chunk.
			if _, err := br.ReadSlice('\n'); err != nil {
				return err
			}
			return nil
		}
		if _, err := io.CopyN(io.Discard, br, size); err != nil {
			return err
		}
		// CRLF after each chunk's data.
		if _, err := br.ReadSlice('\n'); err != nil {
			return err
		}
	}
}
