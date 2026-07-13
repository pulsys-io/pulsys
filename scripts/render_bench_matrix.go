// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build ignore

// Build via:  go run scripts/render_bench_matrix.go scripts/render_bench_matrix_extras.go
//
// Renders the comprehensive head-to-head report from
// tmp/bench/matrix.csv  (produced by scripts/bench_matrix.sh) into:
//
//	docs/results/darwin/throughput.svg
//	docs/results/darwin/rps.svg
//	docs/results/darwin/latency.svg
//	docs/results/darwin/footprint.svg  (if tmp/bench/darwin/footprint.csv)
//	docs/results/darwin/e2e.svg        (if tmp/bench/darwin/e2e_results.csv)
//	docs/results/darwin/summary.md
//
// EC2 uses scripts/render_saturate_charts.go (single-server, high concurrency).
// And summary.md with tables for the README:
//   - throughput cell-by-cell with "x faster"
//   - latency cell-by-cell with "x lower p99"
//   - aggregate win/tie/loss summary
//
// Design policy: two-server head-to-head only; linear axes; bars use
// median with min/max whiskers; summary markdown is generated (not hand-edited).
package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ---- CSV parsing -----------------------------------------------------

type row struct {
	server      string
	payload     string
	scenario    string
	concurrency int
	round       int
	rps         float64
	bps         float64 // bytes per second
	p50us       float64
	p90us       float64
	p99us       float64
	p999us      float64
	totalReqs   int64
	totalBytes  int64
	timeouts    int64
	duration    float64
}

func parseLatency(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0
	}
	var mult float64
	switch {
	case strings.HasSuffix(s, "us"):
		mult, s = 1, strings.TrimSuffix(s, "us")
	case strings.HasSuffix(s, "ms"):
		mult, s = 1000, strings.TrimSuffix(s, "ms")
	case strings.HasSuffix(s, "s"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "s")
	default:
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * mult
}

func parseBytes(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var mult float64
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1e9, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1e6, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1e3, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		mult, s = 1, strings.TrimSuffix(s, "B")
	default:
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * mult
}

func payloadWeight(id string) int64 {
	mult := int64(1)
	last := id[len(id)-1]
	switch last {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	default:
		return 0
	}
	n, err := strconv.ParseInt(id[:len(id)-1], 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}

// payloadsForCharts returns payload sizes shown in README SVGs.
// Oversized shards (64m, 256m) stay in matrix.csv and summary.md
// but are omitted from charts: they crush the y-axis (req/s drops to
// a few thousand) and are outside typical HF chunk traffic (≤16m).
func (a *aggregate) payloadsForCharts() []string {
	const maxChart = 16 << 20 // 16 MiB
	var out []string
	for _, p := range a.payloads {
		if payloadWeight(p) <= maxChart {
			out = append(out, p)
		}
	}
	return out
}

func payloadLabel(id string) string {
	if id == "" {
		return ""
	}
	last := id[len(id)-1]
	rest := id[:len(id)-1]
	switch last {
	case 'k', 'K':
		return rest + " KiB"
	case 'm', 'M':
		return rest + " MiB"
	case 'g', 'G':
		return rest + " GiB"
	default:
		return id
	}
}

// ---- aggregation -----------------------------------------------------

type cell struct {
	rps, bps  []float64
	p50, p99  []float64
	p999      []float64
	totalReqs []int64
}

type key struct {
	server      string
	payload     string
	scenario    string
	concurrency int
}

type aggregate struct {
	cells     map[key]*cell
	payloads  []string
	concs     []int
	scenarios []string
	servers   []string
}

// Optional renderers (implemented in render_bench_matrix_extras.go). Defaults are no-op.
var renderFootprintImpl = func(string, string) {}
var renderE2EImpl = func(string, string) {}

func renderFootprint(csvPath, outPath string) { renderFootprintImpl(csvPath, outPath) }
func renderE2E(csvPath, outPath string)       { renderE2EImpl(csvPath, outPath) }

func newAggregate(rows []row) *aggregate {
	a := &aggregate{cells: map[key]*cell{}}
	seenPay := map[string]bool{}
	seenConc := map[int]bool{}
	seenScen := map[string]bool{}
	seenSrv := map[string]bool{}
	for _, r := range rows {
		k := key{r.server, r.payload, r.scenario, r.concurrency}
		c := a.cells[k]
		if c == nil {
			c = &cell{}
			a.cells[k] = c
		}
		// Honesty: if zero requests completed, the row's rps/bps are
		// wrk's pre-abandonment dribble extrapolation -- discard.
		if r.totalReqs > 0 {
			c.rps = append(c.rps, r.rps)
			c.bps = append(c.bps, r.bps)
			c.p50 = append(c.p50, r.p50us)
			c.p99 = append(c.p99, r.p99us)
			c.p999 = append(c.p999, r.p999us)
		}
		c.totalReqs = append(c.totalReqs, r.totalReqs)
		seenPay[r.payload] = true
		seenConc[r.concurrency] = true
		seenScen[r.scenario] = true
		seenSrv[r.server] = true
	}
	for p := range seenPay {
		a.payloads = append(a.payloads, p)
	}
	sort.Slice(a.payloads, func(i, j int) bool {
		return payloadWeight(a.payloads[i]) < payloadWeight(a.payloads[j])
	})
	for c := range seenConc {
		a.concs = append(a.concs, c)
	}
	sort.Ints(a.concs)
	for s := range seenScen {
		a.scenarios = append(a.scenarios, s)
	}
	sort.Strings(a.scenarios)

	// pulsys first so it gets the left/blue slot consistently.
	if seenSrv["pulsys"] {
		a.servers = append(a.servers, "pulsys")
	}
	for s := range seenSrv {
		if s == "pulsys" {
			continue
		}
		a.servers = append(a.servers, s)
	}
	return a
}

func med(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

func minMax(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return lo, hi
}

// ---- chart paths (darwin vs ec2) ------------------------------------

type benchMeta struct {
	Bench        string `json:"bench"`
	Platform     string `json:"platform"`
	Hostname     string `json:"hostname"`
	VCPU         int    `json:"vcpu"`
	Goos         string `json:"goos"`
	Goarch       string `json:"goarch"`
	Variant      string `json:"variant"`
	InstanceType string `json:"instance_type"`
}

var (
	chartPlatformSubtitle string // e.g. "Darwin arm64 · Mac14,9 · 12 vCPU"
	chartMatrixPath       string
	chartOutDir           string
)

func resolveChartRun() (matrixPath, outDir string) {
	platform := os.Getenv("BENCH_PLATFORM")
	matrixPath = os.Getenv("BENCH_MATRIX")
	outDir = os.Getenv("BENCH_CHARTS_DIR")

	if matrixPath == "" {
		matrixPath = "tmp/bench/darwin/matrix.csv"
	}
	if outDir == "" {
		outDir = "docs/results/darwin"
	}
	if platform != "" && platform != "darwin" {
		log.Fatalf("render_bench_matrix.go is darwin-only (head-to-head vs DingoSpeed). For EC2 use scripts/render_charts.sh ec2.")
	}
	return matrixPath, outDir
}

func loadBenchMeta(matrixPath string) benchMeta {
	metaPath := filepath.Join(filepath.Dir(matrixPath), "matrix.meta.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return benchMeta{}
	}
	var m benchMeta
	if json.Unmarshal(b, &m) != nil {
		return benchMeta{}
	}
	return m
}

func formatPlatformSubtitle(m benchMeta, platform string) string {
	if m.Platform != "" {
		platform = m.Platform
	}
	label := platform
	switch platform {
	case "darwin":
		label = "Darwin (laptop)"
	case "ec2":
		label = "Linux EC2"
	}
	if m.Goos != "" && m.Goarch != "" {
		label = fmt.Sprintf("%s %s/%s", label, m.Goos, m.Goarch)
	}
	// Hostname in meta.json is often the laptop used to render charts, not the EC2 box.
	if m.Hostname != "" && m.Platform != "ec2" && !strings.Contains(m.Hostname, "Mac") && !strings.Contains(m.Hostname, "MBP") {
		label += " · " + m.Hostname
	}
	if m.VCPU > 0 {
		label += fmt.Sprintf(" · %d vCPU", m.VCPU)
	}
	if m.InstanceType != "" {
		label += " · " + m.InstanceType
	}
	if m.Variant != "" && m.Variant != "default" {
		label += " · variant=" + m.Variant
	}
	return label
}

func benchSubtitle(base string) string {
	if chartPlatformSubtitle == "" {
		return base
	}
	return chartPlatformSubtitle + " · " + base
}

func footprintCSV() string {
	if p := os.Getenv("BENCH_FOOTPRINT_CSV"); p != "" {
		return p
	}
	if _, err := os.Stat("tmp/bench/darwin/footprint.csv"); err == nil {
		return "tmp/bench/darwin/footprint.csv"
	}
	return "tmp/bench/footprint.csv"
}

func e2eCSV() string {
	if p := os.Getenv("BENCH_E2E_CSV"); p != "" {
		return p
	}
	if _, err := os.Stat("tmp/bench/darwin/e2e_results.csv"); err == nil {
		return "tmp/bench/darwin/e2e_results.csv"
	}
	return "tmp/bench/e2e_results.csv"
}

// ---- main ------------------------------------------------------------

func main() {
	chartMatrixPath, chartOutDir = resolveChartRun()
	meta := loadBenchMeta(chartMatrixPath)
	chartPlatformSubtitle = formatPlatformSubtitle(meta, os.Getenv("BENCH_PLATFORM"))
	log.Printf("matrix=%s charts=%s subtitle=%q", chartMatrixPath, chartOutDir, chartPlatformSubtitle)

	if err := os.MkdirAll(chartOutDir, 0o755); err != nil {
		log.Fatal(err)
	}

	f, err := os.Open(chartMatrixPath)
	if err != nil {
		log.Fatalf("open matrix: %v", err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		log.Fatalf("read csv: %v", err)
	}
	if len(recs) < 2 {
		log.Fatal("matrix.csv has no data rows")
	}

	var rows []row
	for _, rec := range recs[1:] {
		if len(rec) < 15 {
			continue
		}
		conc, _ := strconv.Atoi(rec[3])
		round, _ := strconv.Atoi(rec[4])
		rps, _ := strconv.ParseFloat(rec[5], 64)
		tot, _ := strconv.ParseInt(rec[11], 10, 64)
		tmo, _ := strconv.ParseInt(rec[13], 10, 64)
		dur, _ := strconv.ParseFloat(rec[14], 64)
		rows = append(rows, row{
			server:      rec[0],
			payload:     rec[1],
			scenario:    rec[2],
			concurrency: conc,
			round:       round,
			rps:         rps,
			bps:         parseBytes(rec[6]),
			p50us:       parseLatency(rec[7]),
			p90us:       parseLatency(rec[8]),
			p99us:       parseLatency(rec[9]),
			p999us:      parseLatency(rec[10]),
			totalReqs:   tot,
			totalBytes:  int64(parseBytes(rec[12])),
			timeouts:    tmo,
			duration:    dur,
		})
	}
	agg := newAggregate(rows)
	if len(agg.servers) < 2 {
		log.Fatalf("need >=2 servers in matrix.csv, got %v", agg.servers)
	}

	// We only render the warm scenario in the SVGs; cold goes into
	// the markdown table only.  The visual chart's job is the
	// steady-state comparison; cold is a TTFB-style metric better
	// expressed as a sentence than a bar.
	renderThroughput(agg, "warm", filepath.Join(chartOutDir, "throughput.svg"))
	renderRPS(agg, "warm", filepath.Join(chartOutDir, "rps.svg"))
	renderLatency(agg, "warm", filepath.Join(chartOutDir, "latency.svg"))
	renderSummary(agg, filepath.Join(chartOutDir, "summary.md"))
	for _, f := range []string{"throughput.svg", "rps.svg", "latency.svg", "summary.md"} {
		fmt.Println("wrote", filepath.Join(chartOutDir, f))
	}

	if fp := footprintCSV(); fp != "" {
		if _, err := os.Stat(fp); err == nil {
			out := filepath.Join(chartOutDir, "footprint.svg")
			renderFootprint(fp, out)
			fmt.Println("wrote", out)
		}
	}
	if e2e := e2eCSV(); e2e != "" {
		if _, err := os.Stat(e2e); err == nil {
			out := filepath.Join(chartOutDir, "e2e.svg")
			renderE2E(e2e, out)
			fmt.Println("wrote", out)
		}
	}
}

// footprint/e2e renderers moved to render_bench_matrix_extras.go to keep
// this orchestration + core matrix chart file under 1k lines.

// ---- multi-panel SVG renderers ---------------------------------------

// renderThroughput emits a 2-column grid of panels (one per
// concurrency level), payload size on the x axis, GB/s on y (linear).
//
// Why a 2-col grid instead of 1xN row: GitHub renders README SVGs at
// at most ~960px content width.  Cramming 5+ panels into one row makes
// each panel ~150px wide -- bars become 1px slivers and axis text
// becomes unreadable.  A 2x3 grid (last cell empty) keeps each panel
// at ~440px so payload labels, bars, and y-axis ticks stay legible.
func renderThroughput(a *aggregate, scenario, path string) {
	if len(a.concs) == 0 {
		return
	}
	chartPay := a.payloadsForCharts()
	var maxBPS float64
	for _, conc := range a.concs {
		for _, p := range chartPay {
			for _, srv := range a.servers {
				c := a.cells[key{srv, p, scenario, conc}]
				if c == nil {
					continue
				}
				_, hi := minMax(c.bps)
				if hi > maxBPS {
					maxBPS = hi
				}
			}
		}
	}
	yMax := niceCeil(maxBPS / 1e9)
	renderGrid(a, scenario, path,
		"Warm-cache throughput, GB/s",
		benchSubtitle("vs DingoSpeed · wrk c=1,4,8… · payloads ≤16 MiB · bar = median GB/s · whisker = min/max"),
		"Higher is better.  pulsys bars are blue.  64m+ in bench_summary.md only.",
		func(b *strings.Builder, conc, ox, oy, w, h int) {
			drawThroughputPanel(b, a, scenario, conc, ox, oy, w, h, yMax, chartPay)
		})
}

func drawThroughputPanel(b *strings.Builder, a *aggregate, scenario string, conc, ox, oy, w, h int, yMax float64, payloads []string) {
	const padTop = 8
	const padBot = 56 // room for rotated payload labels
	const padLeft = 56
	const padRight = 12

	plotX := ox + padLeft
	plotY := oy + padTop
	plotW := w - padLeft - padRight
	plotH := h - padTop - padBot

	gridSteps := 5
	for i := 0; i <= gridSteps; i++ {
		yy := plotY + plotH - i*plotH/gridSteps
		v := float64(i) * yMax / float64(gridSteps)
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#e5e5ea" stroke-width="1"/>`, plotX, plotX+plotW, yy, yy)
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#6e6e73">%.2f</text>`,
			plotX-6, yy+4, v)
	}

	groupCount := len(payloads)
	if groupCount == 0 {
		return
	}
	servers := a.servers
	const groupGap = 10
	groupW := (plotW - groupGap*(groupCount-1)) / groupCount
	barGap := 3
	barW := (groupW - barGap*(len(servers)-1)) / len(servers)
	if barW < 4 {
		barW = 4
	}

	for gi, p := range payloads {
		gx := plotX + gi*(groupW+groupGap)
		// Rotate payload labels -35° so they don't overlap, even
		// with 7+ payload sizes packed into a single panel.
		lx := gx + groupW/2
		ly := plotY + plotH + 14
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#1c1c1e" transform="rotate(-35 %d,%d)">%s</text>`,
			lx, ly, lx, ly, payloadLabel(p))
		for si, srv := range servers {
			c := a.cells[key{srv, p, scenario, conc}]
			if c == nil || len(c.bps) == 0 {
				continue
			}
			medV := med(c.bps) / 1e9
			lo, hi := minMax(c.bps)
			gpsLo := lo / 1e9
			gpsHi := hi / 1e9
			barH := int(medV / yMax * float64(plotH))
			if barH < 1 {
				barH = 1
			}
			x := gx + si*(barW+barGap)
			y := plotY + plotH - barH
			fmt.Fprintf(b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2" fill="%s"/>`,
				x, y, barW, barH, serverColour(srv))
			if len(c.bps) >= 2 {
				yLo := plotY + plotH - int(gpsLo/yMax*float64(plotH))
				yHi := plotY + plotH - int(gpsHi/yMax*float64(plotH))
				cx := x + barW/2
				fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e80" stroke-width="1"/>`,
					cx, cx, yLo, yHi)
			}
		}
	}
	fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e" stroke-width="1.2"/>`,
		plotX, plotX+plotW, plotY+plotH, plotY+plotH)
	fmt.Fprintf(b, `<text x="%d" y="%d" font-size="11" fill="#6e6e73" transform="rotate(-90 %d,%d)">GB/s</text>`,
		ox+16, plotY+plotH/2, ox+16, plotY+plotH/2)
}

// renderRPS plots request-rate on a LINEAR y-axis with per-panel y-max
// fit to the tallest bar in that panel (both servers), so pulsys is
// never clipped with "↑100k" while DingoSpeed stays visible as a
// shorter bar.  Payloads >16 MiB are omitted from charts (see
// payloadsForCharts).
func renderRPS(a *aggregate, scenario, path string) {
	if len(a.concs) == 0 {
		return
	}
	chartPay := a.payloadsForCharts()
	panelMax := perPanelMaxFit(a, scenario, chartPay, func(c *cell) []float64 { return c.rps })
	renderGrid(a, scenario, path,
		"Warm-cache request rate, req/s",
		benchSubtitle("vs DingoSpeed · wrk c=1,4,8… · payloads ≤16 MiB · bar = median req/s · whisker = min/max"),
		"Higher is better.  pulsys bars are blue.  64m+ in bench_summary.md only.",
		func(b *strings.Builder, conc, ox, oy, w, h int) {
			yMax := panelMax[conc]
			if yMax <= 0 {
				yMax = 1
			}
			drawLinearPanel(b, a, scenario, conc, ox, oy, w, h, yMax, chartPay,
				func(c *cell) []float64 { return c.rps }, "req/s",
				formatRPSAxis)
		})
}

// renderLatency: p99 on a LINEAR y-axis; same chart payload cap as RPS.
func renderLatency(a *aggregate, scenario, path string) {
	if len(a.concs) == 0 {
		return
	}
	chartPay := a.payloadsForCharts()
	panelMax := perPanelMaxFit(a, scenario, chartPay, func(c *cell) []float64 { return c.p99 })
	renderGrid(a, scenario, path,
		"Warm-cache p99 latency, µs",
		benchSubtitle("vs DingoSpeed · wrk c=1,4,8… · payloads ≤16 MiB · bar = median p99 · whisker = min/max"),
		"Lower is better.  pulsys bars are blue.  64m+ in bench_summary.md only.",
		func(b *strings.Builder, conc, ox, oy, w, h int) {
			yMax := panelMax[conc]
			if yMax <= 0 {
				yMax = 1
			}
			drawLinearPanel(b, a, scenario, conc, ox, oy, w, h, yMax, chartPay,
				func(c *cell) []float64 { return c.p99 }, "p99 µs",
				formatLatencyAxis)
		})
}

// perPanelMaxFit sets y-max to the tallest median whisker in the panel
// (any server, chart payloads only), with 10% headroom.  Every bar
// fits inside the axis — no clipped "↑100k" pulsys columns.
func perPanelMaxFit(a *aggregate, scenario string, payloads []string, pick func(*cell) []float64) map[int]float64 {
	out := make(map[int]float64, len(a.concs))
	for _, conc := range a.concs {
		var peak float64
		for _, p := range payloads {
			for _, srv := range a.servers {
				c := a.cells[key{srv, p, scenario, conc}]
				if c == nil {
					continue
				}
				_, hi := minMax(pick(c))
				if hi > peak {
					peak = hi
				}
			}
		}
		if peak <= 0 {
			peak = 1
		}
		out[conc] = niceCeil(peak * 1.10)
	}
	return out
}

// formatRPSAxis prints request-rate axis ticks compactly.  When the
// scale is < 10k we keep one decimal so adjacent ticks like 1.6k vs
// 2.0k don't both round to "2k" and look like duplicates.
func formatRPSAxis(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.0fk", v/1e3)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// formatLatencyAxis prints p99 axis ticks compactly with unit-aware
// formatting:
//
//	0          -> "0"           (no unit; tick origin)
//	< 1000us   -> "120µs"
//	< 1000ms   -> "2.5ms" / "20ms"
//	>= 1s      -> "1.5s"
func formatLatencyAxis(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v >= 1e6:
		return fmt.Sprintf("%.1fs", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.0fms", v/1e3)
	case v >= 1e3:
		return fmt.Sprintf("%.1fms", v/1e3)
	default:
		return fmt.Sprintf("%.0fµs", v)
	}
}

// renderGrid lays panels (one per concurrency) out in a 2-column
// grid sized to fit comfortably inside GitHub's README content
// width (~960 px).  Every panel is the same size, so the title +
// subtitle + axis ticks line up across charts.
func renderGrid(a *aggregate, scenario, path, title, subtitle, hint string,
	drawPanel func(b *strings.Builder, conc, ox, oy, w, h int)) {
	const (
		cols    = 2
		panelW  = 440
		panelH  = 280
		padTop  = 110 // room for title + subtitle + hint
		padBot  = 60  // legend + footer
		padLeft = 24
		padRgt  = 24
		gutterX = 24
		gutterY = 36
	)
	rows := (len(a.concs) + cols - 1) / cols
	W := padLeft + padRgt + cols*panelW + (cols-1)*gutterX
	H := padTop + padBot + rows*panelH + (rows-1)*gutterY

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" font-family="-apple-system, BlinkMacSystemFont, 'SF Pro Text', sans-serif" font-size="13" fill="#1c1c1e">`, W, H)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, W, H)
	fmt.Fprintf(&b, `<text x="%d" y="40" font-size="22" font-weight="700">%s  ·  pulsys vs DingoSpeed</text>`, padLeft, title)
	fmt.Fprintf(&b, `<text x="%d" y="64" font-size="13" fill="#6e6e73">%s</text>`, padLeft, subtitle)
	fmt.Fprintf(&b, `<text x="%d" y="84" font-size="12" fill="#6e6e73">%s</text>`, padLeft, hint)

	for i, conc := range a.concs {
		col := i % cols
		row := i / cols
		px := padLeft + col*(panelW+gutterX)
		py := padTop + row*(panelH+gutterY)
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" font-size="14" font-weight="600">concurrency = %d</text>`,
			px+panelW/2, py-10, conc)
		drawPanel(&b, conc, px, py, panelW, panelH)
	}

	// Legend bottom-left.
	legY := H - 32
	for i, srv := range a.servers {
		ix := padLeft + i*150
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="14" height="14" rx="3" fill="%s"/>`, ix, legY, serverColour(srv))
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="13">%s</text>`, ix+20, legY+11, srv)
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#6e6e73">scripts/bench_matrix.sh  ·  scripts/render_bench_matrix.go</text>`,
		W-padRgt, H-12)
	b.WriteString(`</svg>`)
	writeSVG(path, b.String())
}

// drawLinearPanel renders a grouped bar chart with a LINEAR y-axis
// (0..yMax).  Each payload is one x-axis group; each server inside the
// group is one bar.  Whiskers show min/max across rounds.
//
// fmtLabel controls how the y-axis ticks render (e.g. "100k" for RPS,
// "1.0ms" for latency); this keeps the panel function payload-agnostic.
func drawLinearPanel(b *strings.Builder, a *aggregate, scenario string,
	conc, ox, oy, w, h int, yMax float64, payloads []string,
	pick func(*cell) []float64, yLabel string, fmtLabel func(float64) string) {
	const padTop = 8
	const padBot = 56
	const padLeft = 60
	const padRight = 12

	plotX := ox + padLeft
	plotY := oy + padTop
	plotW := w - padLeft - padRight
	plotH := h - padTop - padBot

	if yMax <= 0 {
		yMax = 1
	}

	gridSteps := 5
	for i := 0; i <= gridSteps; i++ {
		yy := plotY + plotH - i*plotH/gridSteps
		v := float64(i) * yMax / float64(gridSteps)
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#e5e5ea" stroke-width="1"/>`, plotX, plotX+plotW, yy, yy)
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#6e6e73">%s</text>`,
			plotX-6, yy+4, fmtLabel(v))
	}

	yOf := func(v float64) int {
		if v <= 0 {
			return plotY + plotH
		}
		if v > yMax {
			v = yMax
		}
		return plotY + plotH - int(v/yMax*float64(plotH))
	}

	groupCount := len(payloads)
	if groupCount == 0 {
		return
	}
	servers := a.servers
	const groupGap = 10
	groupW := (plotW - groupGap*(groupCount-1)) / groupCount
	barGap := 3
	barW := (groupW - barGap*(len(servers)-1)) / len(servers)
	if barW < 4 {
		barW = 4
	}

	for gi, p := range payloads {
		gx := plotX + gi*(groupW+groupGap)
		lx := gx + groupW/2
		ly := plotY + plotH + 14
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#1c1c1e" transform="rotate(-35 %d,%d)">%s</text>`,
			lx, ly, lx, ly, payloadLabel(p))
		for si, srv := range servers {
			c := a.cells[key{srv, p, scenario, conc}]
			if c == nil {
				continue
			}
			samples := pick(c)
			if len(samples) == 0 {
				continue
			}
			medV := med(samples)
			lo, hi := minMax(samples)
			x := gx + si*(barW+barGap)
			y := yOf(medV)
			barH := plotY + plotH - y
			if barH < 1 {
				barH = 1
				y = plotY + plotH - 1
			}
			fmt.Fprintf(b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2" fill="%s"/>`,
				x, y, barW, barH, serverColour(srv))
			if len(samples) >= 2 && hi > 0 {
				yLo := yOf(lo)
				yHi := yOf(hi)
				cx := x + barW/2
				fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e80" stroke-width="1"/>`,
					cx, cx, yLo, yHi)
			}
		}
	}
	fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e" stroke-width="1.2"/>`,
		plotX, plotX+plotW, plotY+plotH, plotY+plotH)
	fmt.Fprintf(b, `<text x="%d" y="%d" font-size="11" fill="#6e6e73" transform="rotate(-90 %d,%d)">%s</text>`,
		ox+16, plotY+plotH/2, ox+16, plotY+plotH/2, yLabel)
}

// ---- markdown summary ------------------------------------------------

func renderSummary(a *aggregate, path string) {
	if len(a.servers) < 2 {
		return
	}
	hf := "pulsys"
	dingo := ""
	for _, s := range a.servers {
		if s != hf {
			dingo = s
			break
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Bench summary\n\n")
	fmt.Fprintf(&b, "_Generated by `scripts/render_bench_matrix.go` from `%s`", chartMatrixPath)
	if chartPlatformSubtitle != "" {
		fmt.Fprintf(&b, " (%s)", chartPlatformSubtitle)
	}
	fmt.Fprintf(&b, "._\n\n")
	fmt.Fprintf(&b, "All numbers are medians across rounds.  `x faster` columns are `pulsys / dingospeed`; values >1 mean pulsys is faster.  Latency `x lower` columns are `dingospeed / pulsys`.\n\n")
	fmt.Fprintf(&b, "README charts include payloads **≤16 MiB** only (typical HF chunk sizes).  **64m** and **256m** rows appear in this table but are omitted from SVGs so req/s axes stay readable.\n\n")

	for _, scenario := range a.scenarios {
		fmt.Fprintf(&b, "## Scenario: `%s`\n\n", scenario)
		fmt.Fprintf(&b, "### Throughput (GB/s) and request rate (req/s)\n\n")
		fmt.Fprintf(&b, "| Concurrency | Payload | %s GB/s | %s GB/s | x faster | %s req/s | %s req/s | x faster |\n",
			hf, dingo, hf, dingo)
		fmt.Fprintf(&b, "|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		var winsThrough, ties, losses int
		for _, conc := range a.concs {
			for _, p := range a.payloads {
				ch := a.cells[key{hf, p, scenario, conc}]
				cd := a.cells[key{dingo, p, scenario, conc}]
				if ch == nil || cd == nil {
					continue
				}
				hG := med(ch.bps) / 1e9
				dG := med(cd.bps) / 1e9
				hR := med(ch.rps)
				dR := med(cd.rps)
				ratio := 0.0
				if dG > 0 {
					ratio = hG / dG
				}
				rRatio := 0.0
				if dR > 0 {
					rRatio = hR / dR
				}
				switch {
				case ratio >= 1.05:
					winsThrough++
				case ratio <= 0.95:
					losses++
				default:
					ties++
				}
				fmt.Fprintf(&b, "| %d | %s | **%.2f** | %.2f | **%.2fx** | **%.0f** | %.0f | **%.2fx** |\n",
					conc, payloadLabel(p), hG, dG, ratio, hR, dR, rRatio)
			}
		}
		fmt.Fprintf(&b, "\n**Throughput record: %d wins, %d ties, %d losses for pulsys** (5%% slack).\n\n",
			winsThrough, ties, losses)

		fmt.Fprintf(&b, "### Latency (microseconds, lower is better)\n\n")
		fmt.Fprintf(&b, "| Concurrency | Payload | %s p50 | %s p50 | %s p99 | %s p99 | p99 x lower |\n",
			hf, dingo, hf, dingo)
		fmt.Fprintf(&b, "|---:|---:|---:|---:|---:|---:|---:|\n")
		var winsLat, tiesLat, lossLat int
		for _, conc := range a.concs {
			for _, p := range a.payloads {
				ch := a.cells[key{hf, p, scenario, conc}]
				cd := a.cells[key{dingo, p, scenario, conc}]
				if ch == nil || cd == nil {
					continue
				}
				h50 := med(ch.p50)
				d50 := med(cd.p50)
				h99 := med(ch.p99)
				d99 := med(cd.p99)
				lr := 0.0
				if h99 > 0 {
					lr = d99 / h99
				}
				switch {
				case lr >= 1.05:
					winsLat++
				case lr <= 0.95 && lr > 0:
					lossLat++
				default:
					tiesLat++
				}
				fmt.Fprintf(&b, "| %d | %s | **%.0f** | %.0f | **%.0f** | %.0f | **%.2fx** |\n",
					conc, payloadLabel(p), h50, d50, h99, d99, lr)
			}
		}
		fmt.Fprintf(&b, "\n**Latency record: %d wins, %d ties, %d losses for pulsys** (5%% slack).\n\n",
			winsLat, tiesLat, lossLat)
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
}

// ---- utilities --------------------------------------------------------

func serverColour(s string) string {
	switch s {
	case "pulsys":
		return "#0A84FF" // primary blue
	case "dingospeed":
		return "#FF9F0A" // amber/orange (color-blind distinct from blue)
	case "direct":
		return "#64D2FF" // cyan baseline
	default:
		return "#8E8E93"
	}
}

func writeSVG(path, s string) {
	if err := os.MkdirAll("docs", 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		log.Fatal(err)
	}
}

func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	// Finer step set than the classic 1/2/2.5/5/10 ladder so e.g.
	// 572ms rounds to 600 (not 1s) and 10.5k rounds to 15k (not
	// 20k).  Keeps the axis tight against the data while still
	// landing on a human-friendly tick interval.
	steps := []float64{1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10}
	mag := 1.0
	for v > 10 {
		v /= 10
		mag *= 10
	}
	for v < 1 {
		v *= 10
		mag /= 10
	}
	for _, s := range steps {
		if s >= v {
			return s * mag
		}
	}
	return 10 * mag
}
