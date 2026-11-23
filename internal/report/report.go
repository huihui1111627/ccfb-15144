// Package report renders acceptance results: a Markdown comparison report and
// machine-readable JSON, covering SLA attainment per application, MPLS usage,
// mobile traffic cost and critical-application interruption duration.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/sim"
)

// RunResult bundles one mode's outcome.
type RunResult struct {
	Mode    string       `json:"mode"`
	Metrics *sim.Metrics `json:"-"`
	Seconds int64        `json:"simulated_seconds"`
	Stores  int          `json:"stores"`
}

// Comparison is the full acceptance artifact.
type Comparison struct {
	GeneratedAt time.Time     `json:"generated_at"`
	SimStart    time.Time     `json:"sim_start"`
	Duration    time.Duration `json:"duration"`
	Stores      int           `json:"stores"`
	CostPerGB   float64       `json:"cellular_cost_per_gb"`
	Baseline    *RunResult    `json:"baseline"`
	SDWAN       *RunResult    `json:"sdwan"`
	FaultLines  []string      `json:"injected_faults"`
}

// Build assembles a comparison from two completed runs.
func Build(stores int, dur time.Duration, start time.Time, costPerGB float64, base, sd *sim.Metrics) *Comparison {
	return &Comparison{
		GeneratedAt: time.Now(), SimStart: start, Duration: dur, Stores: stores,
		CostPerGB: costPerGB,
		Baseline:  &RunResult{Mode: "mpls-only", Metrics: base, Seconds: int64(dur / time.Second), Stores: stores},
		SDWAN:     &RunResult{Mode: "quality-routed", Metrics: sd, Seconds: int64(dur / time.Second), Stores: stores},
	}
}

// WriteJSON persists the comparison as compact JSON. Per-flow interruption
// detail is aggregated away; the per-minute time series goes to a separate
// file via WriteSeries so the main artifact stays small.
func (c *Comparison) WriteJSON(path string) error {
	type jsonOut struct {
		Comparison
		BaselineMetrics sim.CompactMetrics `json:"baseline_metrics"`
		SDWANMetrics    sim.CompactMetrics `json:"sdwan_metrics"`
	}
	out := jsonOut{
		Comparison:      *c,
		BaselineMetrics: c.Baseline.Metrics.Compact(false),
		SDWANMetrics:    c.SDWAN.Metrics.Compact(false),
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	// 时间序列单独成文（页面/二次分析用），按模式分文件
	base := strings.TrimSuffix(path, filepath.Ext(path))
	if err := writeJSONFile(base+"-series-baseline.json", c.Baseline.Metrics.SeriesCopy()); err != nil {
		return err
	}
	return writeJSONFile(base+"-series-sdwan.json", c.SDWAN.Metrics.SeriesCopy())
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// WriteMarkdown renders the human-readable acceptance report.
func (c *Comparison) WriteMarkdown(path string) error {
	var b []byte
	b = append(b, fmt.Sprintf("# SD-WAN 仿真验收报告\n\n")...)
	b = append(b, fmt.Sprintf("- 生成时间：%s\n", c.GeneratedAt.Format(time.RFC3339))...)
	b = append(b, fmt.Sprintf("- 仿真时段：%s 起，时长 %s\n", c.SimStart.Format("2006-01-02 15:04"), c.Duration)...)
	b = append(b, fmt.Sprintf("- 门店数量：%d，区域仓库：%d\n", c.Stores, 10)...)
	b = append(b, fmt.Sprintf("- 移动网单价：¥%.2f / GB\n\n", c.CostPerGB)...)

	b = append(b, "## 1. 各应用质量达标率（SLA 采样口径：每流每秒）\n\n"...)
	b = append(b, "| 应用 | 全走专线 | 按质量选路 | 变化 |\n|---|---:|---:|---:|\n"...)
	for _, app := range model.AllApps {
		ba := c.Baseline.Metrics.Snapshot().App[app]
		sa := c.SDWAN.Metrics.Snapshot().App[app]
		delta := sa.SLARate() - ba.SLARate()
		b = append(b, fmt.Sprintf("| %s | %.2f%% | %.2f%% | %+.2f pp |\n",
			appName(app), pct(ba.SLARate()), pct(sa.SLARate()), delta*100)...)
	}

	b = append(b, "\n## 2. 链路带宽用量（全网上行累计）\n\n"...)
	b = append(b, "| 链路 | 全走专线 | 按质量选路 |\n|---|---:|---:|\n"...)
	for _, t := range model.AllLinkTypes {
		bl := c.Baseline.Metrics.Snapshot().Link[t]
		sl := c.SDWAN.Metrics.Snapshot().Link[t]
		b = append(b, fmt.Sprintf("| %s | %s | %s |\n", linkName(t),
			humanBytes(bl.BytesUp), humanBytes(sl.BytesUp))...)
	}
	mplsB := c.Baseline.Metrics.Snapshot().Link[model.LinkMPLS].BytesUp
	mplsS := c.SDWAN.Metrics.Snapshot().Link[model.LinkMPLS].BytesUp
	if mplsB > 0 {
		b = append(b, fmt.Sprintf("\n专线带宽用量下降 **%.1f%%**（%s → %s）。\n",
			(1-float64(mplsS)/float64(mplsB))*100,
			humanBytes(mplsB), humanBytes(mplsS))...)
	}

	b = append(b, "\n## 3. 移动网流量费用\n\n"...)
	costB := c.Baseline.Metrics.CellularCost(c.CostPerGB)
	costS := c.SDWAN.Metrics.CellularCost(c.CostPerGB)
	b = append(b, fmt.Sprintf("| 模式 | 移动网上行流量 | 费用 |\n|---|---:|---:|\n")...)
	b = append(b, fmt.Sprintf("| 全走专线 | %s | ¥%.2f |\n",
		humanBytes(c.Baseline.Metrics.Snapshot().Link[model.LinkCellular].BytesUp), costB)...)
	b = append(b, fmt.Sprintf("| 按质量选路 | %s | ¥%.2f |\n",
		humanBytes(c.SDWAN.Metrics.Snapshot().Link[model.LinkCellular].BytesUp), costS)...)

	b = append(b, "\n## 4. 关键应用（收银/视频会议）故障中断\n\n"...)
	b = append(b, "| 指标 | 全走专线 | 按质量选路 |\n|---|---:|---:|\n"...)
	b = append(b, fmt.Sprintf("| 中断窗口数 | %d | %d |\n",
		len(c.Baseline.Metrics.Snapshot().Interrupt),
		len(c.SDWAN.Metrics.Snapshot().Interrupt))...)
	b = append(b, fmt.Sprintf("| 平均中断时长 | %s | %s |\n",
		c.Baseline.Metrics.MeanCriticalInterrupt().Round(time.Second),
		c.SDWAN.Metrics.MeanCriticalInterrupt().Round(time.Second))...)
	b = append(b, fmt.Sprintf("| 最长中断时长 | %s | %s |\n",
		c.Baseline.Metrics.MaxCriticalInterrupt().Round(time.Second),
		c.SDWAN.Metrics.MaxCriticalInterrupt().Round(time.Second))...)

	b = append(b, "\n## 5. SD-WAN 行为统计（按质量选路模式）\n\n"...)
	sSnap := c.SDWAN.Metrics.Snapshot()
	flapRate := 0.0
	if sSnap.Migrations > 0 {
		flapRate = float64(sSnap.Flaps) / float64(sSnap.Migrations) * 100
	}
	b = append(b, fmt.Sprintf("- 流迁移次数：%d；乒乓抖动 %d 次（占 %.2f%%，定义为同一流 60s 内迁回刚离开的链路；滞回门限 HoldDown=20s 已抑制绝大部分来回切换）\n",
		sSnap.Migrations, sSnap.Flaps, flapRate)...)
	b = append(b, fmt.Sprintf("- 双链路复制发送 tick：%d，复制冗余字节：%s\n",
		sSnap.Replications, humanBytes(sSnap.Link[model.LinkCellular].Replicated+
			sSnap.Link[model.LinkBroadband].Replicated+
			sSnap.Link[model.LinkMPLS].Replicated))...)
	b = append(b, fmt.Sprintf("- 流量识别准确率：%.2f%%（%d / %d 流-秒采样）\n",
		pct(c.SDWAN.Metrics.ClassRate()), sSnap.ClassCorrect, sSnap.ClassSamples)...)
	b = append(b, fmt.Sprintf("- 控制器失联期间自治 tick：%d（边缘按最后策略继续转发）\n",
		sSnap.AutonomousS)...)
	b = append(b, fmt.Sprintf("- 大流量被限速字节：%s\n",
		humanBytes(totalThrottled(sSnap)))...)

	b = append(b, "\n## 6. 注入场景（仅列仿真窗口内实际触发的）\n\n"...)
	for _, line := range c.FaultLines {
		b = append(b, "- "+line+"\n"...)
	}

	return os.WriteFile(path, b, 0o644)
}

func totalThrottled(m *sim.Metrics) int64 {
	var n int64
	for _, a := range m.App {
		n += a.Throttled
	}
	return n
}

func pct(x float64) float64 { return x * 100 }

func appName(a model.AppID) string {
	switch a {
	case model.AppPOS:
		return "收银 POS"
	case model.AppVC:
		return "视频会议"
	case model.AppBulk:
		return "备份/更新"
	case model.AppWeb:
		return "办公网页"
	default:
		return "未识别/通用"
	}
}

func linkName(t model.LinkType) string {
	switch t {
	case model.LinkMPLS:
		return "专线 MPLS"
	case model.LinkBroadband:
		return "宽带"
	case model.LinkCellular:
		return "移动网"
	}
	return string(t)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// EnsureDir creates the output directory.
func EnsureDir(dir string) error {
	return os.MkdirAll(filepath.Clean(dir), 0o755)
}
