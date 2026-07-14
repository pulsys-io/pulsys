// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package fixtures generates deterministic test artifacts that look
// enough like real Hugging Face files to exercise the proxy +
// huggingface_hub client code paths without bundling large binary
// blobs in the repository.
//
// All generators are seeded from a stable string so the same call
// always returns the same bytes. Tests that need byte-for-byte
// reproducibility can rely on that contract.
package fixtures

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
)

// TokenizerJSON returns the bytes of a minimal HF tokenizer.json that
// huggingface_hub.AutoTokenizer.from_pretrained can parse without
// error. The vocab is intentionally tiny (4 entries) so the file
// stays under 4 KiB and tests run fast.
func TokenizerJSON() []byte {
	doc := map[string]any{
		"version":    "1.0",
		"truncation": nil,
		"padding":    nil,
		"added_tokens": []map[string]any{
			{"id": 0, "content": "<unk>", "special": true},
			{"id": 1, "content": "<pad>", "special": true},
		},
		"normalizer":     map[string]any{"type": "NFC"},
		"pre_tokenizer":  map[string]any{"type": "ByteLevel", "add_prefix_space": false, "trim_offsets": true},
		"post_processor": nil,
		"decoder":        map[string]any{"type": "ByteLevel"},
		"model": map[string]any{
			"type":   "BPE",
			"vocab":  map[string]int{"<unk>": 0, "<pad>": 1, "a": 2, "b": 3},
			"merges": []string{},
		},
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return out
}

// ConfigJSON returns a minimal HF transformers config.json.
func ConfigJSON() []byte {
	doc := map[string]any{
		"architectures":       []string{"GPT2LMHeadModel"},
		"model_type":          "gpt2",
		"hidden_size":         768,
		"num_attention_heads": 12,
		"num_hidden_layers":   12,
		"vocab_size":          50257,
		"tie_word_embeddings": true,
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return out
}

// ReadmeMD returns a non-empty README.md.
func ReadmeMD(name string) []byte {
	return []byte("# " + name + "\n\nFixture model used by pulsys-go test harness.\n")
}

// maxFixtureBody bounds SafetensorsHeader's body so the total
// allocation size cannot overflow int (CWE-190). Fixtures only ever
// need a few KiB; 256 MiB is far above any real test need.
const maxFixtureBody = 256 << 20

// SafetensorsHeader returns a minimal safetensors header (an 8-byte
// little-endian length prefix followed by JSON). The body is N zero
// bytes appended after the header — enough to exercise the proxy's
// streaming + range code paths without committing real model weights.
//
// bodyBytes must be in [0, maxFixtureBody]; anything else is a test bug
// and panics rather than risk an overflowing allocation.
func SafetensorsHeader(bodyBytes int) []byte {
	if bodyBytes < 0 || bodyBytes > maxFixtureBody {
		panic("fixtures: SafetensorsHeader bodyBytes out of range")
	}
	header := map[string]any{
		"__metadata__": map[string]string{"format": "pt"},
		"weight":       map[string]any{"dtype": "F32", "shape": []int{bodyBytes / 4}, "data_offsets": []int{0, bodyBytes}},
	}
	hdrBytes, _ := json.Marshal(header)
	out := make([]byte, 8+len(hdrBytes)+bodyBytes)
	binary.LittleEndian.PutUint64(out[:8], uint64(len(hdrBytes)))
	copy(out[8:8+len(hdrBytes)], hdrBytes)
	// Body is zero-filled.
	return out
}

// GGUFHeader returns a minimal GGUF magic header. The file is not a
// valid GGUF model but contains the magic bytes huggingface_hub's
// gguf_metadata extractor sniffs.
func GGUFHeader() []byte {
	out := make([]byte, 64)
	// GGUF magic: "GGUF" in little-endian uint32.
	out[0], out[1], out[2], out[3] = 'G', 'G', 'U', 'F'
	binary.LittleEndian.PutUint32(out[4:8], 3)   // version
	binary.LittleEndian.PutUint64(out[8:16], 0)  // tensor_count
	binary.LittleEndian.PutUint64(out[16:24], 0) // metadata_kv_count
	return out
}

// ONNXHeader returns a 16-byte ONNX magic header. Not a valid model.
func ONNXHeader() []byte {
	return []byte{0x08, 0x07, 0x12, 0x00, 0x1A, 0x00, 0x22, 0x00,
		0x2A, 0x00, 0x32, 0x00, 0x3A, 0x00, 0x42, 0x00}
}

// LargeLFSPayload returns a deterministic, content-addressed payload
// of approximately size bytes, suitable for LFS upload tests. The
// generator chunks PRBS via SHA-256 of the seed; tests can assert
// byte-identity without storing GB-scale fixtures in the repo.
func LargeLFSPayload(seed string, size int) []byte {
	if size <= 0 {
		return nil
	}
	out := make([]byte, size)
	chunk := sha256.Sum256([]byte(seed))
	for i := 0; i < size; i += 32 {
		end := i + 32
		if end > size {
			end = size
		}
		copy(out[i:end], chunk[:end-i])
		chunk = sha256.Sum256(chunk[:])
	}
	return out
}

// TinyModelFiles returns the canonical set of files that make a repo
// look like a small HF model. Use Map() with SeedRepo helpers on
// mockhub.Server.
func TinyModelFiles(name string) map[string][]byte {
	return map[string][]byte{
		"config.json":       ConfigJSON(),
		"tokenizer.json":    TokenizerJSON(),
		"README.md":         ReadmeMD(name),
		"model.safetensors": SafetensorsHeader(4096), // ~4 KiB body
	}
}
