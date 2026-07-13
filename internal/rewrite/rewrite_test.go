// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package rewrite

import (
	"net/url"
	"strings"
	"testing"
)

func allowHF(host string) bool {
	return strings.HasSuffix(host, "huggingface.co") || strings.HasSuffix(host, "hf.co")
}

func TestLocationToProxy(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:8080")
	got, ok := LocationToProxy(base, allowHF, "https://huggingface.co/gpt2/resolve/main/README.md?download=true")
	if !ok {
		t.Fatal("expected ok")
	}
	if !strings.Contains(got, "/_p/huggingface.co/") || !strings.Contains(got, "download=true") {
		t.Fatalf("bad rewrite: %s", got)
	}
	if _, ok := LocationToProxy(base, allowHF, "https://evil.com/x"); ok {
		t.Fatal("non-allowlisted should fail")
	}
}

func TestLocationToProxyWithOrigin(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:8080")
	got, ok := LocationToProxyWithOrigin(base, allowHF,
		"https://cas-bridge.xethub.hf.co/xet-bridge-us/abc/sha?X-Amz-Date=20260522T000000Z",
		"/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors",
	)
	if !ok {
		t.Fatal("expected ok")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.Path, "/_p/cas-bridge.xethub.hf.co/") {
		t.Fatalf("bad path: %s", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Date") != "20260522T000000Z" {
		t.Fatalf("upstream presign param dropped: %v", q)
	}
	if q.Get(OriginQueryParam) != "/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors" {
		t.Fatalf("origin not attached: %v", q)
	}
}

func TestExtractAndStripOrigin(t *testing.T) {
	in := "X-Amz-Date=20260522T000000Z&" + OriginQueryParam + "=%2FQwen%2FQwen2.5-0.5B%2Fresolve%2Fmain%2Fmodel.safetensors&X-Amz-Signature=abc"
	origin, cleaned := ExtractAndStripOrigin(in)
	if origin != "/Qwen/Qwen2.5-0.5B/resolve/main/model.safetensors" {
		t.Fatalf("origin=%q", origin)
	}
	if strings.Contains(cleaned, OriginQueryParam) {
		t.Fatalf("cleaned still contains origin param: %q", cleaned)
	}
	// Non-origin params preserved.
	if !strings.Contains(cleaned, "X-Amz-Date=20260522T000000Z") || !strings.Contains(cleaned, "X-Amz-Signature=abc") {
		t.Fatalf("dropped non-origin params: %q", cleaned)
	}
}

// Signed CDN queries (CloudFront custom policy on the Xet bridge, S3
// presigns on LFS) cover the resource URL byte-for-byte, including
// query-param ORDER and exact percent-encoding.  The rewrite +
// strip round trip must therefore never reorder or re-encode the
// upstream params -- url.Values.Encode() does both (alphabetical sort,
// re-escape), which the Xet CDN rejects with 403 "invalid resource".
func TestRewriteRoundTrip_PreservesSignedQueryBytes(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:8080")
	// Deliberately non-alphabetical param order and encodings that
	// url.Values round-trips differently (%2A, %27, '+').
	signedQuery := "response-content-disposition=inline%3B+filename%2A%3DUTF-8%27%27model.safetensors" +
		"&X-Xet-Cas-Uid=public" +
		"&Expires=1783981323" +
		"&Policy=eyJTdGF0ZW1lbnQi" +
		"&Signature=MEUCIQCQ" +
		"&Key-Pair-Id=K123" +
		"&user_id=public"
	loc := "https://us.aws.cdn.hf.co/xet-bridge-us/66e8/ddfa?" + signedQuery
	origin := "/Qwen/Qwen2.5-7B-Instruct/resolve/a09a/model-00001-of-00004.safetensors"

	got, ok := LocationToProxyWithOrigin(base, allowHF, loc, origin)
	if !ok {
		t.Fatal("expected ok")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	wantQuery := signedQuery + "&" + OriginQueryParam + "=" + url.QueryEscape(origin)
	if u.RawQuery != wantQuery {
		t.Fatalf("rewrite mutated signed query:\n got  %q\n want %q", u.RawQuery, wantQuery)
	}

	gotOrigin, cleaned := ExtractAndStripOrigin(u.RawQuery)
	if gotOrigin != origin {
		t.Fatalf("origin=%q want %q", gotOrigin, origin)
	}
	if cleaned != signedQuery {
		t.Fatalf("strip mutated signed query:\n got  %q\n want %q", cleaned, signedQuery)
	}
}

func TestExtractAndStripOrigin_Absent(t *testing.T) {
	in := "X-Amz-Date=20260522T000000Z"
	origin, cleaned := ExtractAndStripOrigin(in)
	if origin != "" {
		t.Fatalf("origin=%q", origin)
	}
	if cleaned != in {
		t.Fatalf("cleaned=%q want %q", cleaned, in)
	}
}
