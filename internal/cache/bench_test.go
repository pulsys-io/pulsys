// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package cache

import (
	"bytes"
	"testing"
)

func BenchmarkWriteFullCold(b *testing.B) {
	dir := b.TempDir()
	s, err := NewStore(dir, "none")
	if err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 256*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := KeyHex("GET", "huggingface.co", "/bench/resolve/main/file", string(rune('a'+i%26)), "")
		_, err := s.WriteFullFromStream(key, 200, "huggingface.co", "/bench/resolve/main/file", "", "", "application/octet-stream", bytes.NewReader(payload), int64(len(payload)))
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(payload)), "upstream_bytes/op")
}
