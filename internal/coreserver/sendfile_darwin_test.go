// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build darwin

package coreserver

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestSendFileWithHeaderViaRawDarwin pins the Darwin sf_hdtr fusion
// path: a single sendfile(2) syscall must deliver the response head AND
// the file body to a TCP socket, byte-for-byte intact.
func TestSendFileWithHeaderViaRawDarwin(t *testing.T) {
	dir := t.TempDir()
	body := []byte("0123456789abcdefghijklmnopqrstuv")
	for len(body) < 4096 {
		body = append(body, body...)
	}
	body = body[:4096]
	fpath := filepath.Join(dir, "body.bin")
	if err := os.WriteFile(fpath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(fpath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	header := []byte("HTTP/1.1 200 OK\r\nContent-Length: 4096\r\nConnection: close\r\n\r\n")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type result struct {
		got []byte
		err error
	}
	clientCh := make(chan result, 1)
	go func() {
		c, derr := net.Dial("tcp", ln.Addr().String())
		if derr != nil {
			clientCh <- result{err: derr}
			return
		}
		defer c.Close()
		buf, _ := io.ReadAll(c)
		clientCh <- result{got: buf}
	}()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	tc := conn.(*net.TCPConn)
	rc, err := tc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	transferred, err := sendFileWithHeaderViaRaw(rc, int(f.Fd()), 0, int64(len(body)), header, false /* useCork is Linux-only */, nil /* progress */)
	if err != nil {
		t.Fatalf("sendfile fused: err=%v transferred=%d", err, transferred)
	}
	if transferred != int64(len(body)) {
		t.Fatalf("transferred %d, want %d", transferred, len(body))
	}
	t.Logf("fused returned: transferred=%d (file body bytes only; headers not counted)", transferred)
	conn.Close()

	r := <-clientCh
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := append(append([]byte{}, header...), body...)
	if len(r.got) != len(want) {
		t.Fatalf("client got %d bytes, want %d", len(r.got), len(want))
	}
	for i := range want {
		if r.got[i] != want[i] {
			t.Fatalf("byte %d: got %x want %x", i, r.got[i], want[i])
		}
	}
}
