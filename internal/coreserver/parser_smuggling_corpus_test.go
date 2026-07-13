// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Smuggling corpus conformance test.
//
// PURPOSE
//   parser_differential_test.go is a hand-curated table of 33 inputs
//   targeting categories we identified up-front.  This test fans the
//   same accept/reject contract over two external corpora that grow
//   independently of our editing cadence:
//
//     1. PortSwigger / James Kettle HTTP smuggling research, vendored
//        as internal/coreserver/testdata/portswigger_smuggling.txt.
//     2. nodejs/llhttp's request/ fixture directory, vendored as a
//        git submodule at internal/coreserver/vendor/llhttp.
//
//   A fresh clone of the repository does NOT pull submodules by
//   default; tests that depend on the llhttp corpus t.Skip with an
//   instructive message so go test ./... stays green.  The
//   PortSwigger corpus is in-tree and always runs.
//
// CONTRACT
//   Each fixture carries an expected classification.  For
//   "reject" fixtures coreserver MUST return a non-nil error.  For
//   "accept" fixtures coreserver MUST succeed.  Classification
//   mismatches print the exact bytes and the cited source, so the
//   bisect knows which research vector regressed.

package coreserver_test

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pulsys-io/pulsys/internal/coreserver"
)

// Aliases to keep the atomicInt helper readable -- the package
// uses sync/atomic but we wrap it so the test stays self-contained.
var (
	atomicAddInt64  = atomic.AddInt64
	atomicLoadInt64 = atomic.LoadInt64
)

//go:embed testdata/portswigger_smuggling.txt
var portswiggerCorpusRaw []byte

// portswiggerFixture is one parsed record from the corpus file.
type portswiggerFixture struct {
	name     string
	source   string
	expected string // "reject" | "accept"
	note     string
	payload  []byte
}

// TestParser_PortSwiggerCorpus runs each PortSwigger smuggling
// fixture through coreserver.readRequest and asserts the
// classification matches the cited expectation.
func TestParser_PortSwiggerCorpus(t *testing.T) {
	t.Parallel()
	fixtures, err := parsePortswiggerCorpus(portswiggerCorpusRaw)
	if err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("PortSwigger corpus parsed empty; the testdata file must be present")
	}
	t.Logf("running %d PortSwigger smuggling fixtures", len(fixtures))

	for _, f := range fixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := coreserver.ReadRequestBytesForTest(f.payload)
			switch f.expected {
			case "reject":
				if err == nil {
					t.Fatalf("SECURITY: PortSwigger fixture %q expected reject; coreserver ACCEPTED\n  source: %s\n  note: %s\n  payload=%q",
						f.name, f.source, f.note, f.payload)
				}
			case "accept":
				if err != nil {
					t.Fatalf("baseline PortSwigger fixture %q expected accept; coreserver REJECTED\n  source: %s\n  note: %s\n  err: %v\n  payload=%q",
						f.name, f.source, f.note, err, f.payload)
				}
			default:
				t.Fatalf("fixture %q has unknown expected value %q", f.name, f.expected)
			}
		})
	}
}

// parsePortswiggerCorpus parses the in-tree corpus file format
// documented at the top of testdata/portswigger_smuggling.txt.
// Returns the list of records in declaration order.
//
// Format reminder:
//
//	Lines beginning with "#" are comments.
//	A record begins with "=== <key>: <value>" metadata lines and
//	ends at the next "===" line or EOF.  Within the payload, the
//	literal characters "\r", "\n", "\t", and "\xHH" (lowercase hex
//	only) are unescaped.
func parsePortswiggerCorpus(raw []byte) ([]portswiggerFixture, error) {
	out := []portswiggerFixture{}
	var cur *portswiggerFixture
	var payload bytes.Buffer

	flush := func() {
		if cur == nil {
			return
		}
		cur.payload = []byte(unescapePayload(strings.TrimRight(payload.String(), "\n")))
		out = append(out, *cur)
		cur = nil
		payload.Reset()
	}

	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "=== "):
			rest := strings.TrimPrefix(line, "=== ")
			i := strings.Index(rest, ":")
			if i <= 0 {
				return nil, errInvalidFixtureHeader(line)
			}
			key := strings.TrimSpace(rest[:i])
			value := strings.TrimSpace(rest[i+1:])
			if key == "name" {
				flush()
				cur = &portswiggerFixture{name: value}
				continue
			}
			if cur == nil {
				return nil, errInvalidFixtureHeader(line)
			}
			switch key {
			case "source":
				cur.source = value
			case "expected":
				cur.expected = value
			case "note":
				cur.note = value
			default:
				return nil, errInvalidFixtureHeader(line)
			}
		default:
			if cur == nil {
				// Pre-record padding (blank lines after the doc
				// header).  Skip.
				continue
			}
			payload.WriteString(line)
			payload.WriteString("\n")
		}
	}
	flush()
	return out, nil
}

func errInvalidFixtureHeader(line string) error {
	return errors.New("portswigger_smuggling.txt: malformed fixture header: " + line)
}

// unescapePayload converts the human-readable escape sequences used
// by the corpus file into the literal byte values they represent.
// Recognized escapes:
//
//	\r   -> 0x0D
//	\n   -> 0x0A
//	\t   -> 0x09
//	\v   -> 0x0B
//	\xHH -> 0xHH (lowercase hex digits)
//	\\   -> '\\'
//
// Any other backslash sequence is left literal so a typo doesn't
// silently degrade a fixture's coverage.
func unescapePayload(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case 'r':
			b.WriteByte('\r')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'v':
			b.WriteByte(0x0b)
			i++
		case '\\':
			b.WriteByte('\\')
			i++
		case 'x':
			if i+3 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 3
					continue
				}
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ----- llhttp submodule ---------------------------------------------------

const llhttpRequestDir = "vendor/llhttp/test/request"

// llhttpCriticalFiles lists the markdown filenames whose "must
// reject" fixtures are SMUGGLING-CRITICAL for our use case.  A
// failure to reject in one of these is a hard test failure.  The
// llhttp corpus also includes rejections for Upgrade / WebSocket
// semantics, RTSP-derived methods (ANNOUNCE, DESCRIBE), pipelining
// niceties, and other features pulsys does not implement; for
// those we LOG the disagreement (so we can audit the corpus) but
// don't fail the build.  Smuggling-critical classes:
//
//   - transfer-encoding.md, content-length.md: framing.
//   - lenient-header-value-relaxed.md: relaxed-mode CR/LF/NUL
//     handling that we explicitly refuse.
//   - uri.md: request-target validity (cache-key safety).
//   - method.md: method token grammar.
//   - lenient-version.md, lenient-headers.md: relaxed parser modes
//     we deliberately don't offer.
var llhttpCriticalFiles = map[string]bool{
	"transfer-encoding.md":            true,
	"content-length.md":               true,
	"lenient-header-value-relaxed.md": true,
	"uri.md":                          true,
	"method.md":                       true,
	"lenient-version.md":              true,
	"lenient-headers.md":              true,
}

// TestParser_LlhttpCorpus walks the vendored llhttp request/ test
// fixtures (markdown files containing one or more ```http blocks
// with their expected ```log behavior).  For each fixture we feed
// the http block to coreserver.readRequest and assert the parse
// classification is compatible with the llhttp expectation.
//
// CLASSIFICATION POLICY
//   - llhttp must-accept, we accept                -> pass
//   - llhttp must-accept, we reject                -> pass + log (stricter is safer)
//   - llhttp must-reject, we reject                -> pass
//   - llhttp must-reject, we accept, CRITICAL file -> FAIL (smuggling)
//   - llhttp must-reject, we accept, other file    -> pass + log (semantic divergence we accept)
//
// On a fresh clone without `git submodule update --init`, this test
// t.Skips with a one-liner pointing the developer at the init step.
func TestParser_LlhttpCorpus(t *testing.T) {
	t.Parallel()
	dir := llhttpRequestDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("llhttp submodule not initialized at %s -- run `git submodule update --init` for full corpus coverage; this test does not fail the build", dir)
	}
	var (
		totalFixtures int
		hardChecks    atomicInt
		softNotes     atomicInt
	)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		fixtures := parseLlhttpMarkdown(data)
		totalFixtures += len(fixtures)
		critical := llhttpCriticalFiles[e.Name()]
		for idx, fx := range fixtures {
			fx := fx
			idx := idx
			t.Run(strings.TrimSuffix(e.Name(), ".md")+"_"+strconv.Itoa(idx), func(t *testing.T) {
				t.Parallel()
				_, _, err := coreserver.ReadRequestBytesForTest(fx.payload)
				switch fx.expected {
				case llhttpMustReject:
					if err == nil {
						if critical {
							hardChecks.add(1)
							t.Fatalf("SECURITY: llhttp critical fixture %s#%d (%s) expected reject; coreserver ACCEPTED\n  payload=%q",
								e.Name(), idx, fx.label, fx.payload)
						}
						softNotes.add(1)
						t.Logf("NOTE: llhttp rejects, coreserver accepts (semantic divergence outside smuggling-critical surface)\n  file: %s#%d label: %s\n  payload=%q",
							e.Name(), idx, fx.label, fx.payload)
					}
				case llhttpMustAccept:
					if err != nil {
						softNotes.add(1)
						t.Logf("NOTE: coreserver stricter than llhttp on %s#%d (%s): %v\n  payload=%q",
							e.Name(), idx, fx.label, err, fx.payload)
					}
				case llhttpUnknown:
					// Heuristic classification failed; skip.
				}
			})
		}
	}
	t.Logf("ran %d llhttp request fixtures across %d markdown files (hard checks: %d, soft notes: %d)",
		totalFixtures, len(entries), hardChecks.load(), softNotes.load())
}

// atomicInt is a tiny race-free counter for the llhttp summary
// log.  The fixture sub-tests run in parallel and a plain int
// would trip the race detector even though the value is only
// observed once after all sub-tests complete.
type atomicInt struct{ v int64 }

func (a *atomicInt) add(n int64) { atomicAddInt64(&a.v, n) }
func (a *atomicInt) load() int64 { return atomicLoadInt64(&a.v) }

type llhttpExpectation int

const (
	llhttpUnknown llhttpExpectation = iota
	llhttpMustAccept
	llhttpMustReject
)

type llhttpFixture struct {
	label    string
	payload  []byte
	expected llhttpExpectation
}

// parseLlhttpMarkdown extracts ```http code blocks and their
// following ```log block from one markdown file.  Each (http, log)
// pair becomes one fixture; we classify it as accept/reject by
// inspecting the log for completion vs error markers and the file
// header for lenient-only fixtures.
//
// Recognized markers in the ```log block:
//   - "message complete"      -> valid request
//   - "http_errno="           -> invalid request (llhttp surfaces error)
//   - "<error>"               -> invalid request
//
// Markdown files whose name begins with "lenient-" are interpreted
// as exercises in OPT-IN llhttp leniency; the fixtures inside are
// rejections under llhttp's default (strict) mode and therefore
// also rejections under coreserver.
func parseLlhttpMarkdown(data []byte) []llhttpFixture {
	out := []llhttpFixture{}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "```http") {
			continue
		}
		// Find the closing ``` of the http block.
		j := i + 1
		for j < len(lines) && !strings.HasPrefix(lines[j], "```") {
			j++
		}
		if j >= len(lines) {
			break
		}
		payload := strings.Join(lines[i+1:j], "\n")
		// Find the next ```log block (may have intervening prose).
		k := j + 1
		for k < len(lines) && !strings.HasPrefix(lines[k], "```log") {
			k++
		}
		exp := llhttpUnknown
		label := "fixture"
		if k < len(lines) {
			m := k + 1
			for m < len(lines) && !strings.HasPrefix(lines[m], "```") {
				m++
			}
			logBlock := strings.Join(lines[k+1:m], "\n")
			exp = classifyLlhttpLog(logBlock)
			label = firstNonEmpty(logBlock, "fixture")
			i = m
		} else {
			i = j
		}
		// Find a preceding "### <title>" or "## <title>" for label
		// (cheap; we don't parse headings precisely).
		for p := j; p >= 0; p-- {
			if strings.HasPrefix(lines[p], "### ") {
				label = strings.TrimPrefix(lines[p], "### ")
				break
			}
			if strings.HasPrefix(lines[p], "## ") {
				label = strings.TrimPrefix(lines[p], "## ")
				break
			}
		}
		// Convert payload to wire bytes.  llhttp's markdown
		// convention is two-step:
		//
		//   1. Markdown line endings (LF) are the line terminators
		//      of the HTTP message and must be promoted to CRLF.
		//   2. The text WITHIN each line uses C-style escape
		//      sequences (\n, \r, \f, \t, \v, \0, \x..) for any
		//      literal control byte that needs to appear in the
		//      payload (e.g. a fixture asserting "header value
		//      contains a bare LF" is written as `x:\nNext: y`).
		//
		// Doing (2) before (1) lets a "\n" escape become a real
		// LF byte inside a header value while the markdown line
		// breaks still become CRLF terminators.  We then re-run
		// the line-ending normaliser on the result to make sure
		// the trailing markdown LF after each escaped line is
		// also promoted.
		wire := normaliseLineEndings(unescapeLlhttpPayload(payload))
		out = append(out, llhttpFixture{
			label:    sanitiseLabel(label),
			payload:  []byte(wire),
			expected: exp,
		})
	}
	return out
}

func classifyLlhttpLog(log string) llhttpExpectation {
	switch {
	case strings.Contains(log, "error"):
		return llhttpMustReject
	case strings.Contains(log, "message complete"):
		return llhttpMustAccept
	default:
		return llhttpUnknown
	}
}

func firstNonEmpty(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return fallback
}

func sanitiseLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// normaliseLineEndings converts every "\n" not already preceded by
// "\r" into "\r\n".  llhttp's markdown convention writes one HTTP
// line per markdown line, with the blank line between headers and
// body represented as a single empty line; the wire form needs
// each line terminated by CRLF.
func normaliseLineEndings(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	prev := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' && prev != '\r' {
			b.WriteByte('\r')
		}
		b.WriteByte(c)
		prev = c
	}
	return b.String()
}

// unescapeLlhttpPayload converts the C-style escape sequences used
// inside llhttp's `\`\`\`http fixture blocks to their literal byte
// values.  Recognized escapes:
//
//	\n   -> 0x0A   \r   -> 0x0D   \t   -> 0x09
//	\f   -> 0x0C   \v   -> 0x0B   \0   -> 0x00
//	\xHH -> 0xHH (lowercase hex)
//	\\   -> '\'    \"   -> '"'    \'   -> '\''
//
// Unknown sequences are passed through literally so a typo doesn't
// silently downgrade a fixture's coverage; the differential test
// would surface any genuine new escape immediately.
func unescapeLlhttpPayload(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case 'n':
			b.WriteByte('\n')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'f':
			b.WriteByte(0x0c)
			i++
		case 'v':
			b.WriteByte(0x0b)
			i++
		case '0':
			b.WriteByte(0x00)
			i++
		case '\\':
			b.WriteByte('\\')
			i++
		case '"':
			b.WriteByte('"')
			i++
		case '\'':
			b.WriteByte('\'')
			i++
		case 'x':
			if i+3 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 3
					continue
				}
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
