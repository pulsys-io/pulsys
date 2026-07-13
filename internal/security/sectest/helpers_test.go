// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package sectest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	hffixtures "github.com/pulsys-io/pulsys/internal/hfhub/fixtures"
	"github.com/pulsys-io/pulsys/internal/hfhub/mockhub"
	"github.com/pulsys-io/pulsys/internal/testserver"
)

// cryptoRandomRead is rand.Read; named for clarity in callers that
// want to make the entropy source explicit.
func cryptoRandomRead(b []byte) (int, error) { return rand.Read(b) }

// newStack returns a self-contained pulsys test stack with one
// seeded model (acme/widget) so the proxy's path namespace is
// realistic.  No auth gate: this package tests the protocol
// surface, not the auth contract (which authcontract owns).
func newStack(t *testing.T) *testserver.Stack {
	t.Helper()
	return testserver.New(t, testserver.Config{
		Mock: mockhub.Config{
			Repos: []mockhub.RepoSpec{{
				Name:         "acme/widget",
				InitialFiles: hffixtures.TinyModelFiles("acme/widget"),
			}},
		},
	})
}

// rawDialAndSend opens a fresh TCP connection to addr, writes the
// provided bytes verbatim, and returns up to maxRead bytes from
// the response stream.  If the server closes the connection before
// any bytes arrive, returns ("", nil) -- which is the EXPECTED
// behavior for several smuggling-suspect inputs.
//
// CRITICAL: this helper does NOT go through net/http.Client, so the
// bytes sent are exactly the bytes provided.  Any rewriting (Host
// header injection, CR/LF stripping, etc.) goes through coreserver
// unmodified.  That's the whole point.
func rawDialAndSend(t *testing.T, addr string, payload []byte, maxRead int) (string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		// Servers that close immediately on the parse error race
		// against the write; treat any write error as "connection
		// closed by peer", which is in fact what we expect.
		return "", err
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, maxRead)
	n, err := io.ReadFull(conn, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		// Treat read deadline errors as "no more bytes available",
		// which means we got everything the server is going to send.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return string(buf[:n]), nil
		}
		return string(buf[:n]), err
	}
	return string(buf[:n]), nil
}

// stripAddr trims a leading "http://" prefix and returns the bare
// host:port; useful for raw-TCP dials against testserver.ProxyURL().
func stripAddr(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
}

// parsedResponse is the subset of a *http.Response sectest cares
// about.  We promote resp.Close to an explicit field because
// net/http strips "Connection: close" from resp.Header and only
// surfaces it via the resp.Close boolean -- assertion code that
// looks for headers.Get("Connection") would otherwise silently
// miss it and give false-pass results.
type parsedResponse struct {
	Status    int
	Headers   http.Header
	Body      []byte
	WillClose bool // mirror of http.Response.Close
}

// parseResponse reads one HTTP response from raw bytes.
func parseResponse(raw string) (parsedResponse, error) {
	reader := bufio.NewReader(bytes.NewReader([]byte(raw)))
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return parsedResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return parsedResponse{
		Status:    resp.StatusCode,
		Headers:   resp.Header,
		Body:      body,
		WillClose: resp.Close,
	}, nil
}
