// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package proxy

import (
	"reflect"
	"strings"
	"testing"
)

func TestStripXetLinks(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "real HF response (single header, comma-joined)",
			in: []string{
				`<https://huggingface.co/api/models/openai-community/gpt2/xet-read-token/abc>; rel="xet-auth", <https://cas-server.xethub.hf.co/v1/reconstructions/deadbeef>; rel="xet-reconstruction-info"`,
			},
			want: nil, // every link-value is xet-*; strip the whole header
		},
		{
			name: "mixed: keep non-xet, strip xet",
			in: []string{
				`<https://example.com/page2>; rel="next", <https://huggingface.co/xet-token>; rel="xet-auth"`,
			},
			want: []string{
				`<https://example.com/page2>; rel="next"`,
			},
		},
		{
			name: "multiple Link headers",
			in: []string{
				`<https://example.com/x>; rel="canonical"`,
				`<https://huggingface.co/xet>; rel="xet-reconstruction-info"`,
			},
			want: []string{
				`<https://example.com/x>; rel="canonical"`,
			},
		},
		{
			name: "rel without quotes",
			in: []string{
				`<https://huggingface.co/xet>; rel=xet-auth, <https://example.com/y>; rel=next`,
			},
			want: []string{
				`<https://example.com/y>; rel=next`,
			},
		},
		{
			name: "no Link values at all",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripXetLinks(tc.in)
			// nil and empty slice are equivalent for our purposes.
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stripXetLinks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitLinkValuesHonoursAngleBrackets(t *testing.T) {
	in := `<https://a.example/x?a=1,b=2>; rel="next", <https://b.example/y>; rel="prev"`
	got := splitLinkValues(in)
	want := []string{
		`<https://a.example/x?a=1,b=2>; rel="next"`,
		`<https://b.example/y>; rel="prev"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLinkValueIsXet(t *testing.T) {
	cases := map[string]bool{
		`<x>; rel="xet-auth"`:                  true,
		`<x>; rel="xet-reconstruction-info"`:   true,
		`<x>; rel=xet-auth`:                    true,
		`<x>; rel="next"`:                      false,
		`<x>; rel="canonical"`:                 false,
		strings.ToUpper(`<x>; rel="xet-AUTH"`): true, // case-insensitive
		`<x>; rel="next xet-auth"`:             true, // multi-token rel: any xet-* matches
	}
	for in, want := range cases {
		if got := linkValueIsXet(in); got != want {
			t.Errorf("linkValueIsXet(%q) = %v, want %v", in, got, want)
		}
	}
}
