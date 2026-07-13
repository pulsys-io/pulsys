// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

//go:build ignore

// Build via:  go run scripts/render_bench_svg.go
//
// Reads tmp/bench/results.csv (produced by scripts/bench_compare.sh) and
// emits docs/bench.svg — a grouped-bar chart of warm-hit throughput
// (GB/s decimal — wrk's native unit) by payload size, comparing pulsys
// against Caddy, Go's net/http file server, and Olah (Python/FastAPI HF
// mirror — closest functional peer).  Payload groups are discovered dynamically
// from the CSV so adding 10 MiB / 16 MiB HF-shaped sizes does not require
// hand-editing layout constants.
//
// Rasterise for Markdown previews:
//
//	rsvg-convert -w 2200 docs/bench.svg -o docs/bench.png
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

// parseTransfer turns "8.20GB", "837.54MB", "1.68GB" into bytes/sec (decimal prefixes, matching wrk).
func parseTransfer(s string) float64 {
	s = strings.TrimSpace(s)
	var unit string
	switch {
	case strings.HasSuffix(s, "GB"):
		unit, s = "GB", strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		unit, s = "MB", strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		unit, s = "KB", strings.TrimSuffix(s, "KB")
	default:
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	switch unit {
	case "GB":
		return v * 1e9
	case "MB":
		return v * 1e6
	case "KB":
		return v * 1e3
	}
	return v
}

// payloadWeight maps bench keys like "16k", "256k", "10m", "64m" to a sortable byte-ish weight.
func payloadWeight(id string) int64 {
	switch id {
	case "16k":
		return 16 * 1024
	case "256k":
		return 256 * 1024
	case "4m":
		return 4 << 20
	case "10m":
		return 10 << 20
	case "16m":
		return 16 << 20
	case "64m":
		return 64 << 20
	default:
		return 999 << 40 // unknown payloads sort last but still render
	}
}

func payloadLabel(id string) string {
	switch id {
	case "16k":
		return "16 KiB"
	case "256k":
		return "256 KiB"
	case "4m":
		return "4 MiB"
	case "10m":
		return "10 MiB"
	case "16m":
		return "16 MiB"
	case "64m":
		return "64 MiB"
	default:
		return id
	}
}

func main() {
	f, err := os.Open("tmp/bench/results.csv")
	if err != nil {
		log.Fatalf("open results: %v", err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	rs, err := r.ReadAll()
	if err != nil {
		log.Fatalf("read csv: %v", err)
	}
	if len(rs) < 2 {
		log.Fatal("results.csv has no rows")
	}

	type key struct{ srv, payload string }
	// All bps samples for a (server, payload) pair across multiple
	// bench rounds.  We render a bar at the MEDIAN and a thin whisker
	// from MIN to MAX so the noise floor on sendfile-saturated payload
	// sizes is visually obvious -- bars that overlap with each other's
	// whiskers are statistically tied, no matter which row "won" the
	// last single run.
	bucket := map[key][]float64{}
	for _, rec := range rs[1:] {
		if len(rec) < 4 {
			continue
		}
		srv := rec[0]
		payload := rec[1]
		bps := parseTransfer(rec[3])
		// Honesty check: if the bench harness logged a "completed"
		// column (schema index 7) and zero requests fully finished
		// within the run window, any nonzero bps is "wallpaper"
		// computed from bytes wrk dribbled in before abandoning the
		// socket -- misleading.  Force it to 0 so the chart shows the
		// brutal truth (zero served).
		if len(rec) >= 8 {
			if completed, err := strconv.ParseFloat(rec[7], 64); err == nil && completed == 0 {
				bps = 0
			}
		}
		bucket[key{srv, payload}] = append(bucket[key{srv, payload}], bps)
	}

	// stat returns median, min, max over a non-empty sample.
	stat := func(xs []float64) (med, lo, hi float64) {
		if len(xs) == 0 {
			return 0, 0, 0
		}
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		lo, hi = s[0], s[len(s)-1]
		mid := len(s) / 2
		if len(s)%2 == 1 {
			med = s[mid]
		} else {
			med = (s[mid-1] + s[mid]) / 2
		}
		return
	}

	payloadSet := map[string]struct{}{}
	for k := range bucket {
		payloadSet[k.payload] = struct{}{}
	}
	payloads := make([]string, 0, len(payloadSet))
	for p := range payloadSet {
		payloads = append(payloads, p)
	}
	if len(payloads) == 0 {
		log.Fatal("no payload rows in CSV")
	}
	sort.Slice(payloads, func(i, j int) bool {
		wi, wj := payloadWeight(payloads[i]), payloadWeight(payloads[j])
		if wi != wj {
			return wi < wj
		}
		return payloads[i] < payloads[j]
	})

	servers := []string{"pulsys", "dingospeed", "caddy", "nginx", "go-net-http"}

	gridSteps := 5
	H := 520
	padT, padB := 95, 90
	midGap := len(payloads) - 1
	if midGap < 0 {
		midGap = 0
	}
	perGroupPx := 200 // width budget per payload cluster (four bars inside)
	groupGap := 18
	minPlot := perGroupPx*len(payloads) + groupGap*midGap
	W := padLpad() + padRpad() + minPlot
	padL, padR := padLpad(), padRpad()
	plotW := W - padL - padR
	plotH := H - padT - padB
	groupW := (plotW - groupGap*midGap) / max(1, len(payloads))
	barGap := 3
	barW := (groupW - barGap*(len(servers)-1)) / len(servers)

	maxBPS := 0.0
	for _, srv := range servers {
		for _, p := range payloads {
			samples := bucket[key{srv, p}]
			_, _, hi := stat(samples)
			if hi > maxBPS {
				maxBPS = hi
			}
		}
	}
	yMax := niceCeil(maxBPS / 1e9) // GB/s

	colour := map[string]string{
		"pulsys":      "#0A84FF",
		"dingospeed":  "#FF453A",
		"caddy":       "#8E8E93",
		"nginx":       "#009639",
		"go-net-http": "#B0B0B5",
	}
	label := map[string]string{
		"pulsys":      "pulsys",
		"dingospeed":  "DingoSpeed",
		"caddy":       "Caddy 2.x",
		"nginx":       "nginx",
		"go-net-http": "Go net/http",
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" font-family="-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Segoe UI', Roboto, sans-serif" font-size="12" fill="#1c1c1e">`, W, H)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, W, H)
	fmt.Fprintf(&b, `<text x="%d" y="32" font-size="21" font-weight="700">Warm-hit throughput  ·  pulsys vs DingoSpeed, with Caddy, nginx + Go net/http as kernel-floor reference</text>`, padL)
	fmt.Fprintf(&b, `<text x="%d" y="54" font-size="12" fill="#6e6e73">localhost · wrk -t4 -c64 --timeout 60s · bar = median, whisker = min..max across N rounds · zero-completion rows zeroed</text>`, padL)

	for i := 0; i <= gridSteps; i++ {
		yy := padT + plotH - i*plotH/gridSteps
		v := float64(i) * yMax / float64(gridSteps)
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#e5e5ea" stroke-width="1"/>`, padL, W-padR, yy, yy)
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end" fill="#6e6e73">%.1f</text>`, padL-8, yy+4, v)
	}
	fmt.Fprintf(&b, `<text x="18" y="%d" transform="rotate(-90 18,%d)" font-size="11" fill="#6e6e73">GB/s decimal (wrk)</text>`, padT+plotH/2, padT+plotH/2)

	for gi, p := range payloads {
		groupX := padL + gi*(groupW+groupGap)
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" font-weight="600" font-size="11">%s</text>`,
			groupX+groupW/2, H-padB+18, payloadLabel(p))

		for si, srv := range servers {
			samples := bucket[key{srv, p}]
			if len(samples) == 0 {
				continue
			}
			med, lo, hi := stat(samples)
			gpsMed := med / 1e9
			gpsLo := lo / 1e9
			gpsHi := hi / 1e9

			barH := int(gpsMed / yMax * float64(plotH))
			if barH < 1 {
				barH = 1
			}
			x := groupX + si*(barW+barGap)
			y := padT + plotH - barH
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="3" fill="%s"/>`, x, y, barW, barH, colour[srv])

			// Min/max whisker over the samples (only meaningful when N>=2).
			if len(samples) >= 2 && gpsHi > 0 {
				yLo := padT + plotH - int(gpsLo/yMax*float64(plotH))
				yHi := padT + plotH - int(gpsHi/yMax*float64(plotH))
				cx := x + barW/2
				half := 4
				if barW < 18 {
					half = barW / 3
					if half < 2 {
						half = 2
					}
				}
				whiskerColour := "#1c1c1e80"
				fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="%s" stroke-width="1.2"/>`, cx, cx, yLo, yHi, whiskerColour)
				fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="%s" stroke-width="1.2"/>`, cx-half, cx+half, yLo, yLo, whiskerColour)
				fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="%s" stroke-width="1.2"/>`, cx-half, cx+half, yHi, yHi, whiskerColour)
			}

			fs := 9
			if barW >= 42 {
				fs = 10
			}
			labelY := y - 3
			if len(samples) >= 2 && gpsHi > 0 {
				yHi := padT + plotH - int(gpsHi/yMax*float64(plotH))
				if yHi-3 < labelY {
					labelY = yHi - 3
				}
			}
			fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle" font-size="%d" fill="#1c1c1e">%.2f</text>`,
				x+barW/2, labelY, fs, gpsMed)
		}
	}

	fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#1c1c1e" stroke-width="1.5"/>`,
		padL, W-padR, padT+plotH, padT+plotH)

	legX := W - padR - len(servers)*92
	const legY = 78
	for i, srv := range servers {
		ix := legX + i*92
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="11" height="11" rx="2" fill="%s"/>`, ix, legY, colour[srv])
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11">%s</text>`, ix+14, legY+9, label[srv])
	}

	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" fill="#6e6e73">scripts/bench_compare.sh · scripts/render_bench_svg.go</text>`, padL, H-10)
	b.WriteString(`</svg>`)

	if err := os.MkdirAll("docs", 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("docs/bench.svg", []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote docs/bench.svg")
}

func padLpad() int { return 72 }
func padRpad() int { return 36 }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	steps := []float64{1, 2, 2.5, 5, 10}
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
