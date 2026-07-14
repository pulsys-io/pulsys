// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package upstream forwards requests to Hugging Face infrastructure
// using net/http with response-body streaming, so multi-GB artifact
// downloads never need to be buffered in memory.
//
// Why net/http (and not fasthttp)?
//
//   - Streams response bodies natively via http.Response.Body without
//     additional configuration.  fasthttp.HostClient with
//     StreamResponseBody=true does this too, but the stdlib path has
//     fewer corner cases around chunked + connection reuse on 5xx.
//
//   - Pairs cleanly with the proxy's net/http handler: the server-side
//     io.Copy(w, resp.Body) becomes a single zero-copy splice path on
//     Linux when both ends are *net.TCPConn.
//
//   - No third-party dependency on the production hot path.
//
// The client tunes net/http.Transport for high keep-alive to avoid TLS
// handshakes on every request (the dominant fixed cost when fronting
// Hugging Face's CloudFront origins).
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pulsys-io/pulsys/internal/config"
	"github.com/pulsys-io/pulsys/internal/hostallow"
)

// Response is a transport-agnostic streaming response.
type Response struct {
	Status        int
	Header        http.Header
	ContentLength int64 // -1 if chunked / unknown
	Body          io.ReadCloser
}

// Client is the interface the proxy depends on.  Tests use a fake.
type Client interface {
	// Do issues an HTTPS (or HTTP, depending on cfg.UpstreamScheme)
	// request to scheme://host{path}?{query} and returns a streaming
	// Response.  The caller MUST call Body.Close() to release pooled
	// resources (idle connection back to the pool, response buffer
	// back to net/http's internal pools).
	Do(ctx context.Context, method, host, path, query string, hdr http.Header, body []byte) (*Response, error)
}

// New returns a net/http-backed Client with per-host pooled
// connections.  Tuning rationale:
//
//   - MaxIdleConnsPerHost is set high (256) because Hugging Face
//     downloads are bursty: hf-cli typically opens 8-16 parallel range
//     connections per shard, so we want each to land on a kept-alive
//     conn rather than re-handshake TLS.
//
//   - DisableCompression is true because the proxy strips
//     Accept-Encoding upstream (we cache RAW bytes, see
//     internal/proxy/handler.go) so there's no point negotiating gzip.
//
//   - ResponseHeaderTimeout uses cfg.ReadTimeout so a hung CloudFront
//     origin doesn't pin our goroutine forever; once headers arrive
//     the body stream itself runs without a per-read deadline so we
//     can drain multi-GB shards.
//
//   - ForceAttemptHTTP2 is left off intentionally: HF resolve and
//     LFS/Xet endpoints both serve fine over HTTP/1.1, and HTTP/2
//     introduces stream-level flow control that kills throughput on a
//     single connection compared to multiplexed range fetches over a
//     pool of HTTP/1.1 keep-alives.
func New(cfg *config.Config) Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		MaxConnsPerHost:       512,
		IdleConnTimeout:       cfg.MaxIdle,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: cfg.ReadTimeout,
		DisableCompression:    true,
		WriteBufferSize:       64 << 10,
		ReadBufferSize:        64 << 10,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	hc := &http.Client{
		Transport: tr,
		// Don't follow redirects -- the proxy handler decides per-redirect
		// whether to rewrite Location and bounce the client back through us
		// (multi-host) or follow internally.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &netClient{
		cfg:         cfg,
		hc:          hc,
		tr:          tr,
		allow:       hostallow.New(cfg.AllowHost),
		defaultHost: strings.ToLower(strings.TrimSpace(cfg.DefaultHost)),
	}
}

type netClient struct {
	cfg *config.Config
	hc  *http.Client
	tr  *http.Transport
	// allow is the outbound SSRF gate, re-checked here as defense in
	// depth even though the proxy handler already validates /_p/ hosts.
	allow hostallow.Matcher
	// defaultHost is the operator-configured upstream; it is trusted
	// unconditionally (it may legitimately be a loopback fake under
	// tests/benchmarks) and so bypasses the allowlist.
	defaultHost string
}

// hostAllowed reports whether host may be contacted: the configured
// default upstream is always permitted; any other host (a /_p/ target
// or a rewritten redirect host) must clear the allowlist + SSRF deny
// gate. This mirrors the routing check in internal/proxy so a routing
// regression cannot turn into a server-side request forgery.
func (c *netClient) hostAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h != "" && h == c.defaultHost {
		return true
	}
	return c.allow(h)
}

// hopByHop lists outbound headers we strip on every upstream request.
// Listed lowercase.
var hopByHop = map[string]struct{}{
	"connection": {}, "keep-alive": {}, "proxy-connection": {}, "upgrade": {},
	"te": {}, "trailer": {}, "transfer-encoding": {}, "host": {},
	"proxy-authorization": {},
}

// upstreamURL builds the absolute upstream URL.  Hosts may already
// carry an explicit ":port"; if not, the scheme default is used.
func (c *netClient) upstreamURL(host, path, query string) (*url.URL, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil, errors.New("upstream: empty host")
	}
	// Defense in depth: refuse to build a request to a host that is
	// neither the configured upstream nor on the allowlist. Guards the
	// net/http sink against SSRF if a caller ever passes an
	// externally-influenced host (CWE-918).
	if !c.hostAllowed(host) {
		return nil, fmt.Errorf("upstream: host not allowed: %q", host)
	}
	if path == "" {
		path = "/"
	}
	u := &url.URL{
		Scheme:   c.cfg.UpstreamScheme,
		Host:     host,
		Path:     path,
		RawQuery: query,
	}
	return u, nil
}

func (c *netClient) Do(ctx context.Context, method, host, path, query string, hdr http.Header, body []byte) (*Response, error) {
	u, err := c.upstreamURL(host, path, query)
	if err != nil {
		return nil, err
	}

	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}

	// If the caller's context has no deadline, derive one from the
	// configured read+write timeouts so a runaway upstream can't pin a
	// goroutine forever.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.ReadTimeout+c.cfg.WriteTimeout+30*time.Second)
		// We can't defer cancel here because the body stream outlives Do;
		// instead the cancel runs when the response body is closed.
		req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
		if err != nil {
			cancel()
			return nil, err
		}
		applyHeaders(req, hdr, host)
		resp, err := c.hc.Do(req)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			cancel()
			return nil, fmt.Errorf("upstream: %w", err)
		}
		return wrap(resp, cancel), nil
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, hdr, host)
	resp, err := c.hc.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("upstream: %w", err)
	}
	return wrap(resp, nil), nil
}

// applyHeaders copies caller headers onto req, dropping hop-by-hop
// values and forcing Host to the upstream host (so the upstream sees
// "huggingface.co" or "cas-bridge.xethub.hf.co" etc., not the proxy's
// listen host).
func applyHeaders(req *http.Request, hdr http.Header, host string) {
	for k, vv := range hdr {
		if _, drop := hopByHop[strings.ToLower(k)]; drop {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	req.Host = host
}

// wrap turns an *http.Response into our streaming Response and attaches
// `cancel` (if any) so closing the body releases the per-request
// timeout.
func wrap(resp *http.Response, cancel context.CancelFunc) *Response {
	cl := resp.ContentLength
	body := resp.Body
	if cancel != nil {
		body = &cancelOnCloseReader{rc: body, cancel: cancel}
	}
	return &Response{
		Status:        resp.StatusCode,
		Header:        resp.Header,
		ContentLength: cl,
		Body:          body,
	}
}

// cancelOnCloseReader wraps a response body so the per-request context
// cancel runs exactly once on Close.
type cancelOnCloseReader struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	closed bool
}

func (c *cancelOnCloseReader) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c *cancelOnCloseReader) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.rc.Close()
	c.cancel()
	return err
}
