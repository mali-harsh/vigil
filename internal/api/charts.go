package api

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/store"
)

// Charts are server-rendered SVG: no JS bundle, works in the static export,
// readable without scripts. Hover read-outs are a small progressive extra.

type spark struct {
	Path  string    // latency line
	Area  string    // filled area under it
	Downs []float64 // x of failed checks
	Empty bool
}

// sparkline draws pts (oldest first) into w×h, max latency per bucket.
func sparkline(pts []store.Point, from, to time.Time, w, h float64, buckets int) spark {
	if len(pts) == 0 {
		return spark{Empty: true}
	}
	vals := make([]float64, buckets)
	seen := make([]bool, buckets)
	var downs []float64
	span := float64(to.Sub(from))
	var peak float64 = 1
	for _, p := range pts {
		f := float64(p.At.Sub(from)) / span
		if f < 0 || f > 1 {
			continue
		}
		i := min(int(f*float64(buckets)), buckets-1)
		if p.Down {
			downs = append(downs, f*w)
			continue
		}
		v := float64(p.LatencyMS)
		if !seen[i] || v > vals[i] {
			vals[i] = v
		}
		seen[i] = true
		peak = max(peak, v)
	}
	var line, area strings.Builder
	first, lastX := true, 0.0
	for i := range buckets {
		if !seen[i] {
			continue
		}
		x := (float64(i) + .5) / float64(buckets) * w
		y := h - 2 - vals[i]/peak*(h-4)
		if first {
			fmt.Fprintf(&line, "M%.1f %.1f", x, y)
			fmt.Fprintf(&area, "M%.1f %.1f L%.1f %.1f", x, h, x, y)
			first = false
		} else {
			fmt.Fprintf(&line, " L%.1f %.1f", x, y)
			fmt.Fprintf(&area, " L%.1f %.1f", x, y)
		}
		lastX = x
	}
	if first { // only failures
		return spark{Downs: downs, Empty: len(downs) == 0}
	}
	fmt.Fprintf(&area, " L%.1f %.1f Z", lastX, h)
	return spark{Path: line.String(), Area: area.String(), Downs: downs}
}

// ---- 24h detail chart ----

type tick struct {
	Pos   float64
	Label string
}

type bucket struct {
	T     string `json:"t"`
	Avg   int64  `json:"avg"`
	Max   int64  `json:"max"`
	Fails int    `json:"fails"`
	N     int    `json:"n"`
}

type detailChart struct {
	W, H, Left, Bottom float64
	Avg, MaxLine, Area string
	Fails              []float64
	YTicks, XTicks     []tick
	Data               string // JSON buckets for the hover read-out
	Clipped            bool   // some spikes exceed the scale (exact values in the read-out)
	Empty              bool
}

type stats struct {
	P50, P95, Avg int64
	Checks, Fails int
}

// niceCeil rounds up to 1/2/5×10^n for readable axis labels.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*e >= v {
			return m * e
		}
	}
	return 10 * e
}

// window narrows [from,to] to start at the first point, so a monitor with 10
// minutes of history fills the chart instead of hugging the right edge.
func window(pts []store.Point, from, to time.Time, minSpan time.Duration) time.Time {
	if len(pts) > 0 && pts[0].At.After(from) {
		from = pts[0].At
	}
	if to.Sub(from) < minSpan {
		from = to.Add(-minSpan)
	}
	return from
}

// tickStep picks an x-axis spacing that gives 3–6 labels for the span.
func tickStep(span time.Duration) time.Duration {
	for _, s := range []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour} {
		if span/s <= 6 {
			return s
		}
	}
	return 6 * time.Hour
}

func latencyDetail(res []store.Point, from, to time.Time, loc *time.Location) (detailChart, stats) {
	const W, H, L, B = 1000.0, 240.0, 48.0, 26.0
	c := detailChart{W: W, H: H, Left: L, Bottom: B}
	var st stats
	if len(res) == 0 {
		c.Empty = true
		return c, st
	}
	from = window(res, from, to, 5*time.Minute)
	// ~6 samples per bucket so an "average" is an average; at most 288 (5 min over 24h)
	n := min(max(len(res)/6, 12), 288)
	bs := make([]bucket, n)
	sums := make([]int64, n)
	var lats []int64
	span := float64(to.Sub(from))
	for _, p := range res {
		f := float64(p.At.Sub(from)) / span
		if f < 0 || f > 1 {
			continue
		}
		i := min(int(f*float64(n)), n-1)
		st.Checks++
		if p.Down {
			bs[i].Fails++
			st.Fails++
			continue
		}
		bs[i].N++
		sums[i] += p.LatencyMS
		bs[i].Max = max(bs[i].Max, p.LatencyMS)
		lats = append(lats, p.LatencyMS)
	}
	var maxes []int64
	for i := range bs {
		if bs[i].N > 0 {
			bs[i].Avg = sums[i] / int64(bs[i].N)
			maxes = append(maxes, bs[i].Max)
		}
		bs[i].T = from.Add(time.Duration((float64(i) + .5) * float64(to.Sub(from)) / float64(n))).In(loc).Format("Jan 2 15:04")
	}
	// Scale to the typical range — 2× the 95th-percentile average or the
	// 90th-percentile maximum — so a few cold-start spikes don't flatten the
	// baseline. Taller values are clipped at the top (exact in the read-out).
	var avgs []int64
	for _, b := range bs {
		if b.N > 0 {
			avgs = append(avgs, b.Avg)
		}
	}
	pct := func(v []int64, p int) int64 {
		if len(v) == 0 {
			return 0
		}
		slices.Sort(v)
		return v[min(len(v)-1, len(v)*p/100)]
	}
	peak := max(2*pct(avgs, 95), pct(maxes, 90), 1)
	if len(lats) > 0 {
		slices.Sort(lats)
		st.P50, st.P95 = lats[len(lats)/2], lats[min(len(lats)-1, len(lats)*95/100)]
		var sum int64
		for _, v := range lats {
			sum += v
		}
		st.Avg = sum / int64(len(lats))
	}
	top := niceCeil(float64(peak))
	plotW, plotH := W-L, H-B
	x := func(i int) float64 { return L + (float64(i)+.5)/float64(n)*plotW }
	y := func(v int64) float64 { return plotH - min(float64(v)/top, 1.04)*(plotH-8) }
	var avg, mx, area strings.Builder
	started := false
	lastX := 0.0
	for i, b := range bs {
		if b.N == 0 {
			continue
		}
		cmd := "L"
		if !started {
			cmd = "M"
			fmt.Fprintf(&area, "M%.1f %.1f ", x(i), plotH)
			started = true
		}
		fmt.Fprintf(&avg, "%s%.1f %.1f ", cmd, x(i), y(b.Avg))
		fmt.Fprintf(&mx, "%s%.1f %.1f ", cmd, x(i), y(b.Max))
		fmt.Fprintf(&area, "L%.1f %.1f ", x(i), y(b.Avg))
		lastX = x(i)
	}
	if started {
		fmt.Fprintf(&area, "L%.1f %.1f Z", lastX, plotH)
	}
	c.Avg, c.MaxLine, c.Area = avg.String(), mx.String(), area.String()
	for i, b := range bs {
		if b.Fails > 0 {
			c.Fails = append(c.Fails, x(i))
		}
	}
	for _, b := range bs {
		if float64(b.Max) > top {
			c.Clipped = true
		}
	}
	for _, f := range []float64{0, .5, 1} {
		c.YTicks = append(c.YTicks, tick{Pos: y(int64(top * f)), Label: fmtMS(int64(top * f))})
	}
	step := tickStep(to.Sub(from))
	for t := from.In(loc).Truncate(step).Add(step); t.Before(to); t = t.Add(step) {
		c.XTicks = append(c.XTicks, tick{Pos: L + float64(t.Sub(from))/span*plotW, Label: t.Format("15:04")})
	}
	data, _ := json.Marshal(bs)
	c.Data = string(data)
	return c, st
}

func fmtMS(v int64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fs", float64(v)/1000)
	}
	return fmt.Sprintf("%dms", v)
}
