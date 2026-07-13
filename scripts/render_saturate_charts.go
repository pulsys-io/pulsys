// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build ignore

// render_saturate_charts.go — charts for bench_saturate / pulsys-saturate rows.
//
//	go run scripts/render_saturate_charts.go
//
// Inputs (first match wins):
//
//	SATURATE_MATRIX=path             explicit CSV
//	tmp/bench/ec2/matrix.csv         from scripts/ssm-bench.sh
//	tmp/bench/matrix-saturate.csv    legacy snapshot (still accepted)
//	tmp/bench/ec2/last-run.log       parsed wrk summary lines
//	tmp/bench/saturate.last-run.log  legacy log (still accepted)
//
// Outputs (default docs/results/ec2/; set SATURATE_CHARTS_DIR):
//
//	rps.svg  throughput.svg  latency.svg  cpu.svg
package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const hfBlue = "#0A84FF"

// Color palette for variant series.  Order matters: variants seen first
// in matrix.csv get hfBlue (the existing saturate look), subsequent
// variants get the rest.  Apple-ish palette so the README chart still
// matches the rest of the doc.
var variantPalette = []string{
	"#0A84FF", // saturate (cork on)
	"#FF9F0A", // saturate-no-cork
	"#64D2FF", // saturate-iouring
	"#BF5AF2", // future variants
	"#30D158", // future variants
}

type row struct {
	variant     string // "" for the legacy "pulsys-saturate" baseline.
	payload     string
	scenario    string
	concurrency int
	round       int
	rps         float64
	bps         float64
	p50us       float64
	p99us       float64
	totalReqs   int64
}

type cell struct {
	rps, bps, p50, p99 []float64
}

type saturateAgg struct {
	// cells[variant][payload] -> samples for one (variant, payload) pair.
	cells       map[string]map[string]*cell
	variants    []string // first-seen order, drives legend + color assignment.
	payloads    []string
	concurrency int
	vCPU        string
}

func main() {
	rows, meta := loadSaturateRows()
	if len(rows) == 0 {
		log.Fatal("no saturate rows found — run scripts/ssm-bench.sh or set SATURATE_MATRIX")
	}
	agg := aggregateSaturate(rows)
	if meta.vCPU != "" {
		log.Printf("meta: %d vCPU, conns=%d", mustAtoi(meta.vCPU), agg.concurrency)
	}

	outDir := os.Getenv("SATURATE_CHARTS_DIR")
	if outDir == "" {
		outDir = "docs/results/ec2"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	renderSaturateRPS(agg, meta, filepath.Join(outDir, "rps.svg"))
	renderSaturateThroughput(agg, meta, filepath.Join(outDir, "throughput.svg"))
	renderSaturateLatency(agg, meta, filepath.Join(outDir, "latency.svg"))
	if cpu := loadCPUByPayload(agg.payloads); len(cpu) > 0 {
		p := filepath.Join(outDir, "cpu.svg")
		renderSaturateCPU(cpu, meta, p)
		fmt.Println("wrote", p)
	} else {
		log.Printf("skip cpu.svg (no mpstat logs under tmp/bench/logs/)")
	}
	fmt.Println("wrote", filepath.Join(outDir, "rps.svg"))
	fmt.Println("wrote", filepath.Join(outDir, "throughput.svg"))
	fmt.Println("wrote", filepath.Join(outDir, "latency.svg"))
}

type metaInfo struct {
	vCPU string
}

func loadSaturateRows() ([]row, metaInfo) {
	meta := metaFromLog("tmp/bench/ec2/last-run.log")
	if meta.vCPU == "" {
		meta = metaFromLog("tmp/bench/saturate.last-run.log")
	}
	if p := os.Getenv("SATURATE_MATRIX"); p != "" {
		r, err := readCSVSaturate(p)
		if err != nil {
			log.Fatal(err)
		}
		if len(r) > 0 {
			return r, meta
		}
	}
	for _, p := range []string{
		"tmp/bench/ec2/matrix.csv",
		"tmp/bench/matrix-saturate.csv",
	} {
		r, err := readCSVSaturate(p)
		if err != nil {
			continue
		}
		if len(r) > 0 {
			log.Printf("loaded %d rows from %s", len(r), p)
			return r, meta
		}
	}
	if r, ok := parseSaturateLog("tmp/bench/ec2/last-run.log"); ok {
		log.Printf("loaded %d rows from ec2/last-run.log", len(r))
		return r, meta
	}
	if r, ok := parseSaturateLog("tmp/bench/saturate.last-run.log"); ok {
		log.Printf("loaded %d rows from saturate.last-run.log", len(r))
		return r, meta
	}
	return nil, meta
}

var benchSaturateMeta = regexp.MustCompile(`bench_saturate:\s+(\d+)\s+vCPU`)

func metaFromLog(path string) metaInfo {
	f, err := os.Open(path)
	if err != nil {
		return metaInfo{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := benchSaturateMeta.FindStringSubmatch(sc.Text()); m != nil {
			return metaInfo{vCPU: m[1]}
		}
	}
	return metaInfo{}
}

func readCSVSaturate(path string) ([]row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	var out []row
	for _, rec := range recs[1:] {
		if len(rec) < 12 {
			continue
		}
		// Accept anything matching the saturate family so cork/iouring
		// A/B runs land on the same chart.  Strip the "pulsys-" prefix
		// and treat the bare "pulsys-saturate" as variant "saturate".
		if !strings.HasPrefix(rec[0], "pulsys-saturate") {
			continue
		}
		variant := strings.TrimPrefix(rec[0], "pulsys-")
		conc, _ := strconv.Atoi(rec[3])
		round, _ := strconv.Atoi(rec[4])
		rps, _ := strconv.ParseFloat(rec[5], 64)
		tot, _ := strconv.ParseInt(rec[11], 10, 64)
		if tot <= 0 {
			continue
		}
		out = append(out, row{
			variant: variant,
			payload: rec[1], scenario: rec[2], concurrency: conc, round: round,
			rps: rps, bps: parseBytes(rec[6]),
			p50us: parseLatency(rec[7]), p99us: parseLatency(rec[9]),
			totalReqs: tot,
		})
	}
	return out, nil
}

var logLine = regexp.MustCompile(
	`^pulsys(?:-saturate)?\s+(\S+)\s+(\S+)\s+c=(\d+)\s+r=(\d+)\s+rps=([\d.]+)\s+xfer/s=(\S+)\s+p50=(\S+)\s+p99=(\S+)`)

func parseSaturateLog(path string) ([]row, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var out []row
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := logLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		conc, _ := strconv.Atoi(m[3])
		round, _ := strconv.Atoi(m[4])
		rps, _ := strconv.ParseFloat(m[5], 64)
		out = append(out, row{
			payload: m[1], scenario: m[2], concurrency: conc, round: round,
			rps: rps, bps: parseBytes(m[6]),
			p50us: parseLatency(m[7]), p99us: parseLatency(m[8]),
			totalReqs: 1,
		})
	}
	return out, len(out) > 0
}

func aggregateSaturate(rows []row) *saturateAgg {
	a := &saturateAgg{cells: map[string]map[string]*cell{}}
	seenVariant := map[string]bool{}
	seenPayload := map[string]bool{}
	for _, r := range rows {
		if r.scenario != "" && r.scenario != "warm" {
			continue
		}
		v := r.variant
		if v == "" {
			v = "saturate"
		}
		if !seenVariant[v] {
			seenVariant[v] = true
			a.variants = append(a.variants, v)
		}
		if !seenPayload[r.payload] {
			seenPayload[r.payload] = true
			a.payloads = append(a.payloads, r.payload)
		}
		byPayload := a.cells[v]
		if byPayload == nil {
			byPayload = map[string]*cell{}
			a.cells[v] = byPayload
		}
		c := byPayload[r.payload]
		if c == nil {
			c = &cell{}
			byPayload[r.payload] = c
		}
		c.rps = append(c.rps, r.rps)
		c.bps = append(c.bps, r.bps)
		c.p50 = append(c.p50, r.p50us)
		c.p99 = append(c.p99, r.p99us)
		if r.concurrency > 0 {
			a.concurrency = r.concurrency
		}
	}
	sort.Slice(a.payloads, func(i, j int) bool {
		return payloadWeight(a.payloads[i]) < payloadWeight(a.payloads[j])
	})
	// Stable variant order: saturate always first, then by first-seen.
	sort.SliceStable(a.variants, func(i, j int) bool {
		if a.variants[i] == "saturate" {
			return true
		}
		if a.variants[j] == "saturate" {
			return false
		}
		return false
	})
	return a
}

// variantLabel turns the internal variant key into the legend display.
func variantLabel(v string) string {
	switch v {
	case "saturate":
		return "saturate (cork on)"
	case "saturate-no-cork":
		return "saturate (cork off)"
	case "saturate-iouring":
		return "saturate (io_uring)"
	default:
		return v
	}
}

type cpuPoint struct {
	payload string
	avgBusy float64
}

func loadCPUByPayload(payloads []string) []cpuPoint {
	logDir := "tmp/bench/logs"
	var out []cpuPoint
	for _, p := range payloads {
		matches, _ := filepath.Glob(filepath.Join(logDir, "mpstat_pulsys*_"+p+"_warm_c*.txt"))
		if len(matches) == 0 {
			continue
		}
		var avgs []float64
		for _, f := range matches {
			if v, ok := avgMpstatBusy(f); ok {
				avgs = append(avgs, v)
			}
		}
		if len(avgs) == 0 {
			continue
		}
		out = append(out, cpuPoint{payload: p, avgBusy: med(avgs)})
	}
	sort.Slice(out, func(i, j int) bool {
		return payloadWeight(out[i].payload) < payloadWeight(out[j].payload)
	})
	return out
}

func avgMpstatBusy(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var busy, n float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if !strings.Contains(line, ":") || fields[1] == "CPU" || fields[1] == "all" {
			continue
		}
		idle, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		busy += 100 - idle
		n++
	}
	if n == 0 {
		return 0, false
	}
	return busy / n, true
}

func metaSubtitle(meta metaInfo, agg *saturateAgg) string {
	ncpu := meta.vCPU
	if ncpu == "" {
		ncpu = os.Getenv("SATURATE_VCPU")
	}
	if ncpu == "" {
		ncpu = "?"
	}
	inst := os.Getenv("EC2_INSTANCE_TYPE")
	if inst != "" {
		return fmt.Sprintf("Linux EC2 %s · loopback · wrk c=%d · %s vCPU · bar=median · whisker=min/max",
			inst, agg.concurrency, ncpu)
	}
	return fmt.Sprintf("loopback · wrk c=%d · %s vCPU · bar=median · whisker=min/max",
		agg.concurrency, ncpu)
}

func renderSaturateRPS(agg *saturateAgg, meta metaInfo, path string) {
	yMax := peakMedian(agg, func(c *cell) float64 { return med(c.rps) })
	renderSingleMetric(agg, meta, path,
		"Saturate request rate, req/s",
		metaSubtitle(meta, agg),
		"Higher is better.",
		yMax,
		func(c *cell) float64 { return med(c.rps) },
		func(c *cell) (float64, float64) { return minMax(c.rps) },
		"req/s",
		formatRPSAxis,
	)
}

func renderSaturateThroughput(agg *saturateAgg, meta metaInfo, path string) {
	yMax := peakMedian(agg, func(c *cell) float64 { return med(c.bps) / 1e9 })
	renderSingleMetric(agg, meta, path,
		"Saturate throughput, GB/s",
		metaSubtitle(meta, agg),
		"Higher is better.  wrk Transfer/sec; large payloads may show high GB/s on loopback.",
		yMax,
		func(c *cell) float64 { return med(c.bps) / 1e9 },
		func(c *cell) (float64, float64) {
			lo, hi := minMax(c.bps)
			return lo / 1e9, hi / 1e9
		},
		"GB/s",
		func(v float64) string { return fmt.Sprintf("%.1f", v) },
	)
}

func renderSaturateLatency(agg *saturateAgg, meta metaInfo, path string) {
	yMax := peakMedian(agg, func(c *cell) float64 { return med(c.p99) })
	renderSingleMetric(agg, meta, path,
		"Saturate p99 latency",
		metaSubtitle(meta, agg),
		"Lower is better.",
		yMax,
		func(c *cell) float64 { return med(c.p99) },
		func(c *cell) (float64, float64) { return minMax(c.p99) },
		"p99",
		formatLatencyAxis,
	)
}

func variantColor(i int) string {
	return variantPalette[i%len(variantPalette)]
}

func renderSaturateCPU(points []cpuPoint, meta metaInfo, path string) {
	var peak float64
	for _, p := range points {
		if p.avgBusy > peak {
			peak = p.avgBusy
		}
	}
	yMax := niceCeil(peak * 1.10)
	if yMax > 100 {
		yMax = 100
	}

	const W, H = 720, 420
	const padL, padT, padR, padB = 70, 100, 40, 80
	plotW := W - padL - padR
	plotH := H - padT - padB

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" font-family="-apple-system, BlinkMacSystemFont, 'SF Pro Text', sans-serif" font-size="13" fill="#1c1c1e">`, W, H)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, W, H)
	fmt.Fprintf(&b, `<text x="24" y="40" font-size="22" font-weight="700">Saturate CPU utilization, %% busy (mpstat)</text>`)
	fmt.Fprintf(&b, `<text x="24" y="64" font-size="13" fill="#6e6e73">%s</text>`, metaSubtitle(meta, &saturateAgg{concurrency: mustAtoi(os.Getenv("SATURATE_CONNS"))}))
	fmt.Fprintf(&b, `<text x="24" y="84" font-size="12" fill="#6e6e73">Target ≥80%% on 4k/256k for full-machine demo.</text>`)

	plotX, plotY := padL, padT
	gridSteps := 5
	for i := 0; i <= gridSteps; i++ {
		yy := plotY + plotH - i*plotH/gridSteps
		v := float64(i) * yMax / float64(gridSteps)
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#e5e5ea" stroke-width="1"/>`, plotX, plotX+plotW, yy, yy)
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#6e6e73">%.0f</text>`, plotX-6, yy+4, v)
	}

	n := len(points)
	groupW := plotW / n
	barW := groupW * 55 / 100
	if barW < 24 {
		barW = 24
	}
	for i, p := range points {
		gx := plotX + i*groupW + (groupW-barW)/2
		barH := int(p.avgBusy / yMax * float64(plotH))
		if barH < 1 {
			barH = 1
		}
		y := plotY + plotH - barH
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2" fill="%s"/>`, gx, y, barW, barH, hfBlue)
		lx := gx + barW/2
		ly := plotY + plotH + 14
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" font-size="11" transform="rotate(-35 %d,%d)">%s</text>`,
			lx, ly, lx, ly, payloadLabel(p.payload))
	}
	fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e" stroke-width="1.2"/>`, plotX, plotX+plotW, plotY+plotH, plotY+plotH)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="#6e6e73" transform="rotate(-90 %d,%d)">%% CPU busy</text>`, 18, plotY+plotH/2, 18, plotY+plotH/2)
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#6e6e73">scripts/bench_saturate.sh · render_saturate_charts.go</text>`, W-24, H-12)
	b.WriteString(`</svg>`)
	writeSVG(path, b.String())
}

func renderSingleMetric(
	agg *saturateAgg, meta metaInfo, path, title, subtitle, hint string,
	yMax float64,
	medVal func(*cell) float64,
	rangeVal func(*cell) (float64, float64),
	yLabel string,
	fmtLabel func(float64) string,
) {
	if yMax <= 0 {
		yMax = 1
	}
	yMax = niceCeil(yMax * 1.10)

	// Wider canvas when more than one variant so the grouped bars stay
	// readable.  Single-variant runs render at the original 820x440.
	const padL, padT, padR, padB = 72, 100, 36, 96
	nv := len(agg.variants)
	if nv == 0 {
		nv = 1
	}
	W := 820
	if nv > 1 {
		W = 820 + 120*(nv-1)
	}
	H := 440
	if nv > 1 {
		H += 16 // extra room for the legend row.
	}
	plotW := W - padL - padR
	plotH := H - padT - padB
	plotX, plotY := padL, padT

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" font-family="-apple-system, BlinkMacSystemFont, 'SF Pro Text', sans-serif" font-size="13" fill="#1c1c1e">`, W, H)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, W, H)
	fmt.Fprintf(&b, `<text x="24" y="40" font-size="22" font-weight="700">%s</text>`, title)
	fmt.Fprintf(&b, `<text x="24" y="64" font-size="13" fill="#6e6e73">%s</text>`, subtitle)
	fmt.Fprintf(&b, `<text x="24" y="84" font-size="12" fill="#6e6e73">%s</text>`, hint)

	gridSteps := 5
	for i := 0; i <= gridSteps; i++ {
		yy := plotY + plotH - i*plotH/gridSteps
		v := float64(i) * yMax / float64(gridSteps)
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#e5e5ea" stroke-width="1"/>`, plotX, plotX+plotW, yy, yy)
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#6e6e73">%s</text>`, plotX-6, yy+4, fmtLabel(v))
	}

	payloads := agg.payloads
	n := len(payloads)
	if n == 0 {
		writeSVG(path, b.String())
		return
	}
	variants := agg.variants
	if len(variants) == 0 {
		variants = []string{"saturate"}
	}
	groupW := plotW / n
	// Slot for each variant inside the group, with 1/5 of slot width as
	// padding on each side of the variant cluster.
	clusterW := groupW * 80 / 100
	barW := clusterW / len(variants)
	if barW < 10 {
		barW = 10
	}

	for i, p := range payloads {
		groupX := plotX + i*groupW + (groupW-clusterW)/2
		for vi, v := range variants {
			cell, ok := agg.cells[v][p]
			if !ok || cell == nil {
				continue
			}
			bx := groupX + vi*barW
			medV := medVal(cell)
			lo, hi := rangeVal(cell)
			barH := int(medV / yMax * float64(plotH))
			if barH < 1 && medV > 0 {
				barH = 1
			}
			y := plotY + plotH - barH
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2" fill="%s"/>`,
				bx, y, barW-2, barH, variantColor(vi))
			labelTop := y
			if hi > lo {
				yLo := plotY + plotH - int(lo/yMax*float64(plotH))
				yHi := plotY + plotH - int(hi/yMax*float64(plotH))
				cx := bx + (barW-2)/2
				fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e80" stroke-width="1"/>`, cx, cx, yLo, yHi)
				if yHi < labelTop {
					labelTop = yHi
				}
			}
			// Median value above each bar: with a 4k-vs-16m payload
			// sweep the small bars are sub-pixel on a linear axis, so
			// without the number they read as zero.  Skip when grouped
			// variants make the bars too narrow for the text.
			if barW >= 48 {
				fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" font-size="11" font-weight="600" fill="#1c1c1e">%s</text>`,
					bx+(barW-2)/2, labelTop-6, fmtLabel(medV))
			}
		}
		lx := plotX + i*groupW + groupW/2
		ly := plotY + plotH + 14
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" font-size="11" transform="rotate(-35 %d,%d)">%s</text>`,
			lx, ly, lx, ly, payloadLabel(p))
	}
	fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e" stroke-width="1.2"/>`, plotX, plotX+plotW, plotY+plotH, plotY+plotH)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="#6e6e73" transform="rotate(-90 %d,%d)">%s</text>`, 20, plotY+plotH/2, 20, plotY+plotH/2, yLabel)

	legendY := H - 32
	if len(variants) == 1 {
		fmt.Fprintf(&b, `<rect x="24" y="%d" width="14" height="14" rx="3" fill="%s"/>`, legendY, variantColor(0))
		fmt.Fprintf(&b, `<text x="44" y="%d" font-size="13">%s</text>`, legendY+11, variantLabel(variants[0]))
	} else {
		x := 24
		for vi, v := range variants {
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="14" height="14" rx="3" fill="%s"/>`, x, legendY, variantColor(vi))
			label := variantLabel(v)
			fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="13">%s</text>`, x+20, legendY+11, label)
			x += 22 + 8*len(label) + 14
		}
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" font-size="11" fill="#6e6e73">render_saturate_charts.go</text>`, W-24, H-12)
	b.WriteString(`</svg>`)
	writeSVG(path, b.String())
}

func peakMedian(agg *saturateAgg, f func(*cell) float64) float64 {
	var peak float64
	for _, variant := range agg.variants {
		for _, p := range agg.payloads {
			c := agg.cells[variant][p]
			if c == nil {
				continue
			}
			if v := f(c); v > peak {
				peak = v
			}
		}
	}
	return peak
}

func med(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
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
	if id == "" {
		return 0
	}
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

func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
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
	best := steps[len(steps)-1] * mag
	for _, s := range steps {
		if s >= v {
			best = s * mag
			break
		}
	}
	return best
}

func writeSVG(path, s string) {
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		log.Fatal(err)
	}
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
