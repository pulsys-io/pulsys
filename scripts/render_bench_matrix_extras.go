// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build bench_matrix_extras

package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ---- footprint renderer ---------------------------------------------

type footRow struct {
	server  string
	payload string
	rssMB   float64
	cpuS    float64
}

func init() {
	renderFootprintImpl = renderFootprintReal
	renderE2EImpl = renderE2EReal
}

func parseCPUTime(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Forms: "12.345" (plain seconds), "0:00.43" m:ss.frac,
	// "1:23.45" m:ss.frac, "1:23:45" h:mm:ss, "1-12:34:56" d-h:mm:ss.
	days := 0.0
	if i := strings.Index(s, "-"); i >= 0 {
		d, _ := strconv.ParseFloat(s[:i], 64)
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	secs := 0.0
	switch len(parts) {
	case 3:
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		sec, _ := strconv.ParseFloat(parts[2], 64)
		secs = h*3600 + m*60 + sec
	case 2:
		m, _ := strconv.ParseFloat(parts[0], 64)
		sec, _ := strconv.ParseFloat(parts[1], 64)
		secs = m*60 + sec
	default:
		secs, _ = strconv.ParseFloat(parts[0], 64)
	}
	return days*86400 + secs
}

func renderFootprintReal(csvPath, outPath string) {
	f, err := os.Open(csvPath)
	if err != nil {
		log.Printf("footprint: %v", err)
		return
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		log.Printf("footprint csv: %v", err)
		return
	}
	type k struct{ srv, pay string }
	peakRSS := map[k]float64{}
	finalCPU := map[k]float64{}
	servers := map[string]bool{}
	payloads := map[string]bool{}
	for _, rec := range recs[1:] {
		if len(rec) < 5 {
			continue
		}
		rss, _ := strconv.ParseFloat(rec[3], 64)
		cpu := parseCPUTime(rec[4])
		key := k{rec[0], rec[1]}
		if rss > peakRSS[key] {
			peakRSS[key] = rss
		}
		if cpu > finalCPU[key] {
			finalCPU[key] = cpu
		}
		servers[rec[0]] = true
		payloads[rec[1]] = true
	}
	if len(servers) < 2 {
		return
	}
	payList := make([]string, 0, len(payloads))
	for p := range payloads {
		payList = append(payList, p)
	}
	sort.Slice(payList, func(i, j int) bool {
		return payloadWeightExtra(payList[i]) < payloadWeightExtra(payList[j])
	})
	srvList := []string{}
	if servers["pulsys"] {
		srvList = append(srvList, "pulsys")
	}
	for s := range servers {
		if s != "pulsys" {
			srvList = append(srvList, s)
		}
	}

	// Two-panel SVG: RSS (linear MB) | CPU (linear seconds)
	const (
		panelW = 360
		panelH = 220
		padT   = 90
		padB   = 80
		padL   = 70
		padR   = 32
		gutter = 40
	)
	W := padL + padR + 2*panelW + gutter
	H := padT + padB + panelH

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" font-family="-apple-system, BlinkMacSystemFont, 'SF Pro Text', sans-serif" font-size="11" fill="#1c1c1e">`, W, H)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, W, H)
	fmt.Fprintf(&b, `<text x="%d" y="32" font-size="20" font-weight="700">Sidecar footprint  ·  pulsys vs DingoSpeed</text>`, padL)
	fmt.Fprintf(&b, `<text x="%d" y="54" font-size="11" fill="#6e6e73">20s sustained wrk -t4 -c8 warm hammer · RSS = peak resident set · CPU = cumulative process time</text>`, padL)
	fmt.Fprintf(&b, `<text x="%d" y="72" font-size="10" fill="#6e6e73">Lower is better on BOTH axes.</text>`, padL)

	// Panel 1: RSS
	maxRSS := 0.0
	for _, v := range peakRSS {
		if v > maxRSS {
			maxRSS = v
		}
	}
	yMaxRSS := niceCeilExtra(maxRSS / 100)
	drawFootBarPanel(&b, padL, padT, panelW, panelH, "Peak RSS (MB)", payList, srvList,
		func(srv, pay string) float64 { return peakRSS[k{srv, pay}] / 100 }, yMaxRSS, 100, "MB")

	// Panel 2: CPU
	maxCPU := 0.0
	for _, v := range finalCPU {
		if v > maxCPU {
			maxCPU = v
		}
	}
	yMaxCPU := niceCeilExtra(maxCPU / 10)
	drawFootBarPanel(&b, padL+panelW+gutter, padT, panelW, panelH, "Cumulative CPU (s)", payList, srvList,
		func(srv, pay string) float64 { return finalCPU[k{srv, pay}] / 10 }, yMaxCPU, 10, "s")

	legX := padL
	legY := H - 28
	for i, srv := range srvList {
		ix := legX + i*150
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="11" height="11" rx="2" fill="%s"/>`, ix, legY, serverColourExtra(srv))
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11">%s</text>`, ix+15, legY+9, srv)
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" fill="#6e6e73">scripts/bench_footprint.sh  ·  scripts/render_bench_matrix.go</text>`,
		W-padR-400, H-10)
	b.WriteString(`</svg>`)
	writeSVGExtra(outPath, b.String())
}

// drawFootBarPanel: shared two-server grouped-bar panel.  values are
// already pre-scaled by 1/scale; yMax is also in pre-scaled units.
func drawFootBarPanel(b *strings.Builder, ox, oy, w, h int, title string,
	payloads, servers []string, val func(srv, pay string) float64,
	yMax, scale float64, unit string) {
	const padTop = 24
	const padBot = 40
	const padLeft = 50
	const padRight = 8

	fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" font-size="11" font-weight="600">%s</text>`,
		ox+w/2, oy+14, title)

	plotX := ox + padLeft
	plotY := oy + padTop
	plotW := w - padLeft - padRight
	plotH := h - padTop - padBot

	gridSteps := 5
	for i := 0; i <= gridSteps; i++ {
		yy := plotY + plotH - i*plotH/gridSteps
		v := float64(i) * yMax / float64(gridSteps) * scale
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#e5e5ea" stroke-width="1"/>`, plotX, plotX+plotW, yy, yy)
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="end" font-size="9" fill="#6e6e73">%.0f%s</text>`,
			plotX-4, yy+3, v, unit)
	}

	groupCount := len(payloads)
	if groupCount == 0 {
		return
	}
	const groupGap = 14
	groupW := (plotW - groupGap*(groupCount-1)) / groupCount
	barGap := 4
	barW := (groupW - barGap*(len(servers)-1)) / len(servers)

	for gi, p := range payloads {
		gx := plotX + gi*(groupW+groupGap)
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" font-size="10" fill="#1c1c1e">%s</text>`,
			gx+groupW/2, plotY+plotH+14, payloadLabelExtra(p))
		for si, srv := range servers {
			v := val(srv, p)
			if v <= 0 {
				continue
			}
			barH := int(v / yMax * float64(plotH))
			if barH < 1 {
				barH = 1
			}
			if barH > plotH {
				barH = plotH
			}
			x := gx + si*(barW+barGap)
			y := plotY + plotH - barH
			fmt.Fprintf(b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2" fill="%s"/>`,
				x, y, barW, barH, serverColourExtra(srv))
			// Label inside or above the bar.
			labelY := y - 2
			fs := 9
			fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" font-size="%d" fill="#1c1c1e">%.0f%s</text>`,
				x+barW/2, labelY, fs, v*scale, unit)
		}
	}
	fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e" stroke-width="1.2"/>`,
		plotX, plotX+plotW, plotY+plotH, plotY+plotH)
}

// ---- e2e renderer ----------------------------------------------------

func renderE2EReal(csvPath, outPath string) {
	f, err := os.Open(csvPath)
	if err != nil {
		log.Printf("e2e: %v", err)
		return
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return
	}
	// Group key: "upstream|proxy|scenario" -> []wall_s.  Stringified
	// so we can pass the map to helper funcs without anonymous-struct
	// type-identity headaches.
	bucket := map[string][]float64{}
	mk := func(u, p, s string) string { return u + "|" + p + "|" + s }
	upstreams := map[string]bool{}
	proxies := map[string]bool{}
	scens := map[string]bool{}
	for _, rec := range recs[1:] {
		if len(rec) < 5 {
			continue
		}
		w, _ := strconv.ParseFloat(rec[3], 64)
		key := mk(rec[0], rec[1], rec[2])
		bucket[key] = append(bucket[key], w)
		upstreams[rec[0]] = true
		proxies[rec[1]] = true
		scens[rec[2]] = true
	}
	if len(proxies) == 0 {
		return
	}
	upList := []string{"loopback", "remote"}
	scenList := []string{"cold", "warm"}
	// pulsys first, then dingospeed, then direct as a thin baseline.
	proxyList := []string{}
	for _, p := range []string{"pulsys", "dingospeed", "direct"} {
		if proxies[p] {
			proxyList = append(proxyList, p)
		}
	}

	// 2 upstreams x 2 scenarios = 4 grouped bars per upstream-scenario cell.
	// Layout: 2 panels (one per upstream), each a 2-group bar (cold/warm).
	const (
		panelW = 420
		panelH = 230
		padT   = 90
		padB   = 80
		padL   = 70
		padR   = 32
		gutter = 40
	)
	nPanels := 0
	for _, u := range upList {
		if upstreams[u] {
			nPanels++
		}
	}
	if nPanels == 0 {
		return
	}
	W := padL + padR + nPanels*panelW + (nPanels-1)*gutter
	H := padT + padB + panelH

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" font-family="-apple-system, BlinkMacSystemFont, 'SF Pro Text', sans-serif" font-size="11" fill="#1c1c1e">`, W, H)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, W, H)
	fmt.Fprintf(&b, `<text x="%d" y="32" font-size="20" font-weight="700">End-to-end `+"`hf download`"+` wallclock  ·  pulsys vs DingoSpeed</text>`, padL)
	fmt.Fprintf(&b, `<text x="%d" y="54" font-size="11" fill="#6e6e73">768 MiB synthetic model · hf download --max-workers 8 · bar = median wallclock seconds across rounds</text>`, padL)
	fmt.Fprintf(&b, `<text x="%d" y="72" font-size="10" fill="#6e6e73">Lower is better.  Direct = no proxy in the path.</text>`, padL)

	// Compute global yMax across panels for like-axes.  Excludes the
	// "direct" cold/warm rows on remote: those are the upstream-bound
	// floor (~24s) and would crush the proxy bars off the chart.  We
	// still show them in the panel, but cap yMax at the proxy max.
	proxyMax := 0.0
	for _, u := range upList {
		if !upstreams[u] {
			continue
		}
		for _, sc := range scenList {
			for _, pr := range proxyList {
				if pr == "direct" && u == "remote" {
					continue
				}
				vals := bucket[mk(u, pr, sc)]
				if len(vals) == 0 {
					continue
				}
				m := medExtra(vals)
				if m > proxyMax {
					proxyMax = m
				}
			}
		}
	}
	yMax := niceCeilExtra(proxyMax)

	pi := 0
	for _, u := range upList {
		if !upstreams[u] {
			continue
		}
		px := padL + pi*(panelW+gutter)
		py := padT
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" font-size="11" font-weight="600">upstream = %s</text>`,
			px+panelW/2, py-6, u)
		drawE2EPanel(&b, bucket, mk, u, scenList, proxyList, px, py, panelW, panelH, yMax)
		pi++
	}

	legX := padL
	legY := H - 28
	for i, srv := range proxyList {
		ix := legX + i*150
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="11" height="11" rx="2" fill="%s"/>`, ix, legY, serverColourExtra(srv))
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11">%s</text>`, ix+15, legY+9, srv)
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" fill="#6e6e73">scripts/bench_e2e.sh  ·  scripts/render_bench_matrix.go</text>`,
		W-padR-360, H-10)
	b.WriteString(`</svg>`)
	writeSVGExtra(outPath, b.String())
}

func drawE2EPanel(b *strings.Builder, bucket map[string][]float64,
	mk func(string, string, string) string,
	upstream string, scens, proxies []string,
	ox, oy, w, h int, yMax float64) {
	const padTop = 8
	const padBot = 40
	const padLeft = 50
	const padRight = 8

	plotX := ox + padLeft
	plotY := oy + padTop
	plotW := w - padLeft - padRight
	plotH := h - padTop - padBot

	gridSteps := 5
	for i := 0; i <= gridSteps; i++ {
		yy := plotY + plotH - i*plotH/gridSteps
		v := float64(i) * yMax / float64(gridSteps)
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#e5e5ea" stroke-width="1"/>`, plotX, plotX+plotW, yy, yy)
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="end" font-size="9" fill="#6e6e73">%.1fs</text>`,
			plotX-4, yy+3, v)
	}

	groupCount := len(scens)
	const groupGap = 30
	groupW := (plotW - groupGap*(groupCount-1)) / groupCount
	barGap := 4
	barW := (groupW - barGap*(len(proxies)-1)) / len(proxies)

	for gi, sc := range scens {
		gx := plotX + gi*(groupW+groupGap)
		fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" font-size="10" font-weight="600" fill="#1c1c1e">%s</text>`,
			gx+groupW/2, plotY+plotH+16, sc)
		for si, pr := range proxies {
			vals := bucket[mk(upstream, pr, sc)]
			if len(vals) == 0 {
				continue
			}
			medV := medExtra(vals)
			lo, hi := minMaxExtra(vals)
			barH := int(medV / yMax * float64(plotH))
			if barH < 1 {
				barH = 1
			}
			if barH > plotH {
				barH = plotH
			}
			x := gx + si*(barW+barGap)
			y := plotY + plotH - barH
			fmt.Fprintf(b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2" fill="%s"/>`,
				x, y, barW, barH, serverColourExtra(pr))
			// Whisker -- clipped to chart bounds so off-scale "direct
			// remote" bars don't drag the whisker through the title.
			if len(vals) >= 2 {
				yLo := plotY + plotH - int(lo/yMax*float64(plotH))
				yHi := plotY + plotH - int(hi/yMax*float64(plotH))
				if yHi < plotY {
					yHi = plotY
				}
				if yLo < plotY {
					yLo = plotY
				}
				cx := x + barW/2
				fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e80" stroke-width="1"/>`,
					cx, cx, yLo, yHi)
			}
			fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" font-size="9" fill="#1c1c1e">%.2fs</text>`,
				x+barW/2, y-2, medV)
		}
	}
	fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e" stroke-width="1.2"/>`,
		plotX, plotX+plotW, plotY+plotH, plotY+plotH)
}

func payloadWeightExtra(id string) int64 {
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

func payloadLabelExtra(id string) string {
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

func medExtra(xs []float64) float64 {
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

func minMaxExtra(xs []float64) (float64, float64) {
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

func serverColourExtra(s string) string {
	switch s {
	case "pulsys":
		return "#0A84FF"
	case "dingospeed":
		return "#FF9F0A"
	case "direct":
		return "#64D2FF"
	default:
		return "#8E8E93"
	}
}

func writeSVGExtra(path, s string) {
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		log.Fatal(err)
	}
}

func niceCeilExtra(v float64) float64 {
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
	for _, s := range steps {
		if s >= v {
			return s * mag
		}
	}
	return 10 * mag
}
