// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package fixtures

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestTokenizerParses(t *testing.T) {
	bs := TokenizerJSON()
	if len(bs) == 0 {
		t.Fatal("empty")
	}
	var v map[string]any
	if err := json.Unmarshal(bs, &v); err != nil {
		t.Fatalf("tokenizer.json is invalid JSON: %v", err)
	}
}

func TestConfigParses(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(ConfigJSON(), &v); err != nil {
		t.Fatal(err)
	}
	if v["model_type"] != "gpt2" {
		t.Fatalf("model_type=%v", v["model_type"])
	}
}

func TestSafetensorsHeader(t *testing.T) {
	bs := SafetensorsHeader(4096)
	if len(bs) < 16+4096 {
		t.Fatalf("len=%d", len(bs))
	}
	// First 8 bytes = LE uint64 of header JSON length.
	hdrLen := int(bs[0]) | int(bs[1])<<8 | int(bs[2])<<16 | int(bs[3])<<24
	if hdrLen <= 0 || hdrLen > 4096 {
		t.Fatalf("bad header len: %d", hdrLen)
	}
	var v map[string]any
	if err := json.Unmarshal(bs[8:8+hdrLen], &v); err != nil {
		t.Fatalf("header is invalid JSON: %v", err)
	}
}

func TestLargeLFSPayloadDeterministic(t *testing.T) {
	a := LargeLFSPayload("seed-1", 8192)
	b := LargeLFSPayload("seed-1", 8192)
	if !bytes.Equal(a, b) {
		t.Fatal("generator is not deterministic for same seed")
	}
	c := LargeLFSPayload("seed-2", 8192)
	if bytes.Equal(a, c) {
		t.Fatal("different seeds should produce different output")
	}
	if len(a) != 8192 {
		t.Fatalf("len=%d", len(a))
	}
}

func TestGGUFMagic(t *testing.T) {
	bs := GGUFHeader()
	if string(bs[:4]) != "GGUF" {
		t.Fatalf("magic=%q", bs[:4])
	}
}

func TestTinyModelFiles(t *testing.T) {
	files := TinyModelFiles("acme/widget")
	for _, k := range []string{"config.json", "tokenizer.json", "README.md", "model.safetensors"} {
		if len(files[k]) == 0 {
			t.Fatalf("missing %s", k)
		}
	}
}
