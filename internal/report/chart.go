package report

import (
	"fmt"
	"html/template"
	"math"
	"strings"

	"github.com/ksi512/BrainX-prototype/internal/store"
)

// 图表全部在 Go 侧算好、输出内联 SVG。
//
// 为什么不引图表库：单文件看板的第一条硬约束是「断网可用、零外部资源」。
// 引一个 CDN 上的图表库，页面一断网就只剩空白——而看板恰恰是在
// 「磁盘/网络出问题时」最需要打开的东西。

const (
	chartW     = 940
	chartH     = 260
	chartPadL  = 46
	chartPadR  = 46
	chartPadT  = 16
	chartPadB  = 30
	chartInner = chartW - chartPadL - chartPadR
)

// chart 生成近 30 天双轴图：柱=每日入库（左轴）、线=每日召回（右轴）、均值虚线。
func chart(st store.Stats) template.HTML {
	if len(st.Days) == 0 {
		return template.HTML(`<p class="empty">没有数据</p>`)
	}
	plotH := float64(chartH - chartPadT - chartPadB)
	const ticks = 4
	// 轴上限向上取整到 ticks 的倍数：直接用最大值会让刻度变成 6/5/3/2/0
	// 这种抖动序列——读数的人第一眼就会怀疑数据错了。
	maxImports := niceMax(atLeast(st.MaxImports, 1), ticks)
	maxRecalls := niceMax(atLeast(st.MaxRecalls, 1), ticks)
	n := len(st.Days)
	step := float64(chartInner) / float64(n)
	barW := math.Max(4, step*0.55)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" width="100%%" height="%d" role="img" aria-label="近 30 天入库与召回">`, chartW, chartH, chartH)

	// 网格与左轴刻度
	for i := 0; i <= ticks; i++ {
		y := float64(chartPadT) + plotH*float64(ticks-i)/float64(ticks)
		value := int(math.Round(maxImports * float64(i) / float64(ticks)))
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#e8ebef" stroke-width="1"/>`, chartPadL, y, chartW-chartPadR, y)
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" text-anchor="end" class="axis">%d</text>`, chartPadL-8, y+4, value)
	}
	// 右轴刻度（召回）
	for i := 0; i <= ticks; i++ {
		y := float64(chartPadT) + plotH*float64(ticks-i)/float64(ticks)
		value := int(math.Round(maxRecalls * float64(i) / float64(ticks)))
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" text-anchor="start" class="axis axis-right">%d</text>`, chartW-chartPadR+8, y+4, value)
	}

	// 柱：每日入库
	b.WriteString(`<g class="series-imports">`)
	for i, day := range st.Days {
		h := plotH * float64(day.Imports) / maxImports
		x := float64(chartPadL) + step*float64(i) + (step-barW)/2
		y := float64(chartPadT) + plotH - h
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="1.5" fill="#246bce" fill-opacity="0.78"><title>%s 入库 %d</title></rect>`,
			x, y, barW, math.Max(h, 0.6), day.Label, day.Imports)
	}
	b.WriteString(`</g>`)

	// 折线：每日召回
	var points []string
	b.WriteString(`<g class="series-recalls">`)
	for i, day := range st.Days {
		y := float64(chartPadT) + plotH - plotH*float64(day.Recalls)/maxRecalls
		x := float64(chartPadL) + step*float64(i) + step/2
		points = append(points, fmt.Sprintf("%.1f,%.1f", x, y))
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.6" fill="#b45309"><title>%s 召回 %d</title></circle>`, x, y, day.Label, day.Recalls)
	}
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#b45309" stroke-width="1.8" stroke-linejoin="round"/>`, strings.Join(points, " "))
	b.WriteString(`</g>`)

	// 均值虚线
	meanY := float64(chartPadT) + plotH - plotH*st.ImportMean/maxImports
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#246bce" stroke-width="1" stroke-dasharray="4 3" opacity="0.6"><title>入库均值 %.1f/天</title></line>`,
		chartPadL, meanY, chartW-chartPadR, meanY, st.ImportMean)
	meanRecallY := float64(chartPadT) + plotH - plotH*st.RecallMean/maxRecalls
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#b45309" stroke-width="1" stroke-dasharray="2 3" opacity="0.6"><title>召回均值 %.1f/天</title></line>`,
		chartPadL, meanRecallY, chartW-chartPadR, meanRecallY, st.RecallMean)

	// x 轴标签：每 5 天一个
	for i, day := range st.Days {
		if i%5 != 0 && i != n-1 {
			continue
		}
		x := float64(chartPadL) + step*float64(i) + step/2
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" text-anchor="middle" class="axis">%s</text>`, x, chartH-10, day.Label)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// niceMax 把轴上限抬到 ticks 的整数倍，保证刻度落在整数上。
func niceMax(value, ticks int) float64 {
	step := math.Ceil(float64(value) / float64(ticks))
	if step < 1 {
		step = 1
	}
	return step * float64(ticks)
}

func atLeast(value, floor int) int {
	if value < floor {
		return floor
	}
	return value
}

// bar 是资产构成的一段横条。用 CSS 宽度而不是 SVG：横条不需要坐标轴。
func bar(item store.KindStat) template.HTML {
	if item.Share < 1 {
		item.Share = 1
	}
	if item.Share > 100 {
		item.Share = 100
	}
	return template.HTML(fmt.Sprintf(
		`<div class="bar-row"><code>%s</code><div class="track"><div class="fill" style="width:%d%%"></div></div><span class="num-sm">%d</span><span class="muted">%d%%</span></div>`,
		template.HTMLEscapeString(item.Name), item.Share, item.Count, item.Share))
}

// pct 把比例压成可显示的整数百分比。
func pct(part, total int) int {
	if total <= 0 {
		return 0
	}
	value := part * 100 / total
	if value > 100 {
		return 100
	}
	return value
}

func daysAgo(ms int64) string {
	if ms == 0 {
		return "从未"
	}
	days := (store.NowMillis() - ms) / int64(24*60*60*1000)
	switch {
	case days <= 0:
		return "今天"
	case days == 1:
		return "昨天"
	default:
		return fmt.Sprintf("%d 天前", days)
	}
}
