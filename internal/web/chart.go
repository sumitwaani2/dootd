package web

import (
	"fmt"
	"html"
	"math"
	"strings"
	"time"
)

// Line is one chart series. NaN values are gaps.
type Line struct {
	Name   string
	Color  string
	Dashed bool
	Values []float64
}

// Chart is a server-rendered SVG line chart (no JavaScript).
type Chart struct {
	Times    []time.Time
	Lines    []Line
	From, To time.Time
	Gap      time.Duration // points further apart are not joined
	Format   func(float64) string
	MinTop   float64 // smallest y-axis maximum (e.g. 100 for percent)
	Bytes    bool    // round the axis in binary units (MB, GB)
}

const (
	chartW, chartH         = 640.0, 180.0
	padL, padR, padT, padB = 58.0, 10.0, 26.0, 20.0
)

// SVG renders the chart.
func (c Chart) SVG() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" font-family="system-ui,sans-serif" font-size="11">`,
		chartW, chartH, chartW, chartH)
	b.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/>`)

	top := c.MinTop
	for _, l := range c.Lines {
		for _, v := range l.Values {
			if !math.IsNaN(v) && v > top {
				top = v
			}
		}
	}
	if c.Bytes {
		unit := float64(1 << 10)
		for _, u := range []float64{1 << 30, 1 << 20} {
			if top >= u {
				unit = u
				break
			}
		}
		top = niceCeil(top/unit) * unit
	} else {
		top = niceCeil(top)
	}
	plotW, plotH := chartW-padL-padR, chartH-padT-padB
	span := c.To.Sub(c.From).Seconds()
	x := func(t time.Time) float64 { return padL + plotW*t.Sub(c.From).Seconds()/span }
	y := func(v float64) float64 { return padT + plotH*(1-v/top) }

	// Grid and y labels.
	for i := 0; i <= 4; i++ {
		v := top * float64(i) / 4
		yy := y(v)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e5e7eb" stroke-width="1"/>`, padL, yy, chartW-padR, yy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="end" fill="#6b7280">%s</text>`, padL-6, yy+4, esc(c.Format(v)))
	}
	// Time labels.
	layout := "15:04"
	if c.To.Sub(c.From) > 36*time.Hour {
		layout = "Jan 2"
	}
	for i := 0; i <= 3; i++ {
		t := c.From.Add(time.Duration(float64(c.To.Sub(c.From)) * float64(i) / 3))
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		} else if i == 3 {
			anchor = "end"
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="%s" fill="#6b7280">%s</text>`, x(t), chartH-5, anchor, t.Local().Format(layout))
	}

	// Series.
	hasData := false
	for _, l := range c.Lines {
		var path, dots strings.Builder
		pen := false
		var prev time.Time
		segLen := 0
		var segX, segY float64
		endSeg := func() {
			if segLen == 1 { // a lone point would be invisible as a path
				fmt.Fprintf(&dots, `<circle cx="%.1f" cy="%.1f" r="2" fill="%s"/>`, segX, segY, l.Color)
			}
			segLen = 0
		}
		for i, v := range l.Values {
			if i >= len(c.Times) {
				break
			}
			t := c.Times[i]
			if math.IsNaN(v) || t.Before(c.From) {
				if pen {
					endSeg()
				}
				pen = false
				continue
			}
			if pen && c.Gap > 0 && t.Sub(prev) > c.Gap {
				endSeg()
				pen = false
			}
			cmd := "L"
			if !pen {
				cmd = "M"
			}
			fmt.Fprintf(&path, "%s%.1f %.1f", cmd, x(t), y(v))
			pen, prev = true, t
			segLen++
			segX, segY = x(t), y(v)
			if !l.Dashed {
				hasData = true
			}
		}
		if pen {
			endSeg()
		}
		if path.Len() == 0 {
			continue
		}
		b.WriteString(dots.String())
		dash := ""
		if l.Dashed {
			dash = ` stroke-dasharray="4 3"`
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="1.6" stroke-linejoin="round"%s/>`, path.String(), l.Color, dash)
	}
	if !hasData {
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#9ca3af" font-size="13">No data for this period yet</text>`,
			padL+plotW/2, padT+plotH/2)
	}

	// Legend with the latest value of each line.
	lx := padL
	for _, l := range c.Lines {
		label := l.Name
		if v, ok := lastValue(l.Values); ok {
			label += " " + c.Format(v)
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="8" width="10" height="10" fill="%s"/>`, lx, l.Color)
		fmt.Fprintf(&b, `<text x="%.1f" y="17" fill="#1d2330">%s</text>`, lx+14, esc(label))
		lx += 14 + float64(len(label))*6.2 + 14
	}
	b.WriteString(`</svg>`)
	return []byte(b.String())
}

func lastValue(vs []float64) (float64, bool) {
	for i := len(vs) - 1; i >= 0; i-- {
		if !math.IsNaN(vs[i]) {
			return vs[i], true
		}
	}
	return 0, false
}

// niceCeil rounds v up to 1, 2, 2.5 or 5 times a power of ten.
func niceCeil(v float64) float64 {
	if v <= 0 || math.IsNaN(v) {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

func esc(s string) string { return html.EscapeString(s) }

func fmtPct(v float64) string { return fmt.Sprintf("%.0f%%", v) }

func fmtBytes(v float64) string {
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.1f GB", v/(1<<30))
	case v >= 1<<20:
		if v < 10<<20 && v != float64(int64(v/(1<<20)))*(1<<20) {
			return fmt.Sprintf("%.1f MB", v/(1<<20))
		}
		return fmt.Sprintf("%.0f MB", v/(1<<20))
	case v >= 1<<10:
		return fmt.Sprintf("%.0f KB", v/(1<<10))
	}
	return fmt.Sprintf("%.0f B", v)
}

func fmtNum(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("%.1fk", v/1000)
	case v >= 10 || v == 0:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func fmtMs(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1f s", v/1000)
	}
	return fmt.Sprintf("%.0f ms", v)
}
