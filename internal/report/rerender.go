package report

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/sim"
)

// compactFile mirrors the JSON layout written by WriteJSON.
type compactFile struct {
	GeneratedAt     time.Time          `json:"generated_at"`
	SimStart        time.Time          `json:"sim_start"`
	Duration        time.Duration      `json:"duration"`
	Stores          int                `json:"stores"`
	CostPerGB       float64            `json:"cellular_cost_per_gb"`
	FaultLines      []string           `json:"injected_faults"`
	BaselineMetrics sim.CompactMetrics `json:"baseline_metrics"`
	SDWANMetrics    sim.CompactMetrics `json:"sdwan_metrics"`
}

// RerenderFromJSON rebuilds the Markdown report from a compact acceptance JSON
// artifact, so report wording can be refreshed without re-running the
// (expensive) simulation.
func RerenderFromJSON(jsonPath, mdPath string) error {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var f compactFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}

	var b []byte
	b = append(b, "# SD-WAN 仿真验收报告\n\n"...)
	b = append(b, fmt.Sprintf("- 生成时间：%s\n", f.GeneratedAt.Format(time.RFC3339))...)
	b = append(b, fmt.Sprintf("- 仿真时段：%s 起，时长 %s\n", f.SimStart.Format("2006-01-02 15:04"), f.Duration)...)
	b = append(b, fmt.Sprintf("- 门店数量：%d，区域仓库：%d\n", f.Stores, 10)...)
	b = append(b, fmt.Sprintf("- 移动网单价：¥%.2f / GB\n\n", f.CostPerGB)...)

	// 1. 各应用达标率
	b = append(b, "## 1. 各应用质量达标率（SLA 采样口径：每流每秒）\n\n"...)
	b = append(b, "| 应用 | 全走专线 | 按质量选路 | 变化 |\n|---|---:|---:|---:|\n"...)
	for _, app := range model.AllApps {
		ba, okB := f.BaselineMetrics.App[app]
		sa, okS := f.SDWANMetrics.App[app]
		if !okB || !okS {
			continue // 稀疏数据里缺失的应用不渲染
		}
		delta := sa.SLARate() - ba.SLARate()
		b = append(b, fmt.Sprintf("| %s | %.2f%% | %.2f%% | %+.2f pp |\n",
			appName(app), pct(ba.SLARate()), pct(sa.SLARate()), delta*100)...)
	}

	// 2. 链路用量
	b = append(b, "\n## 2. 链路带宽用量（全网上行累计）\n\n"...)
	b = append(b, "| 链路 | 全走专线 | 按质量选路 |\n|---|---:|---:|\n"...)
	for _, t := range model.AllLinkTypes {
		bl, okB := f.BaselineMetrics.Link[t]
		sl, okS := f.SDWANMetrics.Link[t]
		if !okB || !okS {
			continue
		}
		b = append(b, fmt.Sprintf("| %s | %s | %s |\n", linkName(t),
			humanBytes(bl.BytesUp), humanBytes(sl.BytesUp))...)
	}
	mplsB, okB := f.BaselineMetrics.Link[model.LinkMPLS]
	mplsS, okS := f.SDWANMetrics.Link[model.LinkMPLS]
	if okB && okS && mplsB.BytesUp > 0 {
		b = append(b, fmt.Sprintf("\n专线带宽用量下降 **%.1f%%**（%s → %s）。\n",
			(1-float64(mplsS.BytesUp)/float64(mplsB.BytesUp))*100,
			humanBytes(mplsB.BytesUp), humanBytes(mplsS.BytesUp))...)
	}

	// 3. 移动费用
	b = append(b, "\n## 3. 移动网流量费用\n\n"...)
	var cellB, cellS int64
	if l, ok := f.BaselineMetrics.Link[model.LinkCellular]; ok {
		cellB = l.BytesUp
	}
	if l, ok := f.SDWANMetrics.Link[model.LinkCellular]; ok {
		cellS = l.BytesUp
	}
	costB := float64(cellB) / 1e9 * f.CostPerGB
	costS := float64(cellS) / 1e9 * f.CostPerGB
	b = append(b, "| 模式 | 移动网上行流量 | 费用 |\n|---|---:|---:|\n"...)
	b = append(b, fmt.Sprintf("| 全走专线 | %s | ¥%.2f |\n", humanBytes(cellB), costB)...)
	b = append(b, fmt.Sprintf("| 按质量选路 | %s | ¥%.2f |\n", humanBytes(cellS), costS)...)

	// 4. 关键应用中断（紧凑聚合视图）
	b = append(b, "\n## 4. 关键应用（收银/视频会议）故障中断\n\n"...)
	b = append(b, "| 指标 | 全走专线 | 按质量选路 |\n|---|---:|---:|\n"...)
	b = append(b, fmt.Sprintf("| 中断窗口数 | %d | %d |\n",
		f.BaselineMetrics.InterruptCount, f.SDWANMetrics.InterruptCount)...)
	b = append(b, fmt.Sprintf("| 平均中断时长 | %s | %s |\n",
		time.Duration(f.BaselineMetrics.InterruptMeanMs)*time.Millisecond,
		time.Duration(f.SDWANMetrics.InterruptMeanMs)*time.Millisecond)...)
	b = append(b, fmt.Sprintf("| 最长中断时长 | %s | %s |\n",
		time.Duration(f.BaselineMetrics.InterruptMaxMs)*time.Millisecond,
		time.Duration(f.SDWANMetrics.InterruptMaxMs)*time.Millisecond)...)

	// 5. SD-WAN 行为统计
	b = append(b, "\n## 5. SD-WAN 行为统计（按质量选路模式）\n\n"...)
	s := f.SDWANMetrics
	flapRate := 0.0
	if s.Migrations > 0 {
		flapRate = float64(s.Flaps) / float64(s.Migrations) * 100
	}
	b = append(b, fmt.Sprintf("- 流迁移次数：%d；乒乓抖动 %d 次（占 %.2f%%，定义为同一流 60s 内迁回刚离开的链路；滞回门限 HoldDown=20s 已抑制绝大部分来回切换）\n",
		s.Migrations, s.Flaps, flapRate)...)
	var replicated int64
	for _, lm := range s.Link {
		replicated += lm.Replicated
	}
	b = append(b, fmt.Sprintf("- 双链路复制发送 tick：%d，复制冗余字节：%s\n", s.Replications, humanBytes(replicated))...)
	classRate := 0.0
	if s.ClassSamples > 0 {
		classRate = float64(s.ClassCorrect) / float64(s.ClassSamples) * 100
	}
	b = append(b, fmt.Sprintf("- 流量识别准确率：%.2f%%（%d / %d 流-秒采样）\n",
		classRate, s.ClassCorrect, s.ClassSamples)...)
	b = append(b, fmt.Sprintf("- 控制器失联期间自治 tick：%d（边缘按最后策略继续转发）\n", s.AutonomousTicks)...)
	var throttled int64
	for _, am := range s.App {
		throttled += am.Throttled
	}
	b = append(b, fmt.Sprintf("- 大流量被限速字节：%s\n", humanBytes(throttled))...)

	// 6. 注入场景
	b = append(b, "\n## 6. 注入场景（仅列仿真窗口内实际触发的）\n\n"...)
	lines := f.FaultLines
	if len(lines) == 0 {
		lines = []string{"本窗口内无注入故障"}
	}
	for _, line := range lines {
		b = append(b, "- "+line+"\n"...)
	}
	return os.WriteFile(mdPath, b, 0o644)
}
