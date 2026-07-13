// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package testserver wires a complete in-process Pulsys test stack:
// mock Hub + pulsys + temp cache dir. The harness is reusable from
// any package that wants to exercise the proxy end-to-end against a
// deterministic upstream.
//
// Example:
//
//	stack := testserver.New(t, testserver.Config{
//	    Repos: []mockhub.RepoSpec{{
//	        Name: "acme/widget",
//	        InitialFiles: fixtures.TinyModelFiles("acme/widget"),
//	    }},
//	})
//	resp, err := http.Get(stack.ProxyURL() + "/acme/widget/resolve/main/config.json")
package testserver
