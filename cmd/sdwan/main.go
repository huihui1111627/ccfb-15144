// Command sdwan runs the SD-WAN simulation suite:
//
//	sdwan accept -stores 300 -days 3   # 几百门店跑数天的对照验收
//	sdwan serve -addr :8080            # 实时仪表盘（包级仿真）
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/pprof"
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
	"sdwan/internal/report"
	"sdwan/internal/sim"
	"sdwan/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "accept":
		runAccept(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "quick":
		runQuick(os.Args[2:])
	case "rerender":
		runRerender(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sdwan — 软件定义广域网仿真系统

用法:
  sdwan accept    跑全量验收仿真（全走专线 vs 按质量选路对照）
  sdwan quick     小规模快速冒烟验证
  sdwan serve     启动实时仪表盘（包级仿真 + 故障注入）
  sdwan rerender  从已有 acceptance.json 重新生成 Markdown（不重跑仿真）

参数见各子命令 -h。
`)
}

func runAccept(args []string) {
	fs := flag.NewFlagSet("accept", flag.ExitOnError)
	stores := fs.Int("stores", 300, "仿真门店数量")
	days := fs.Float64("days", 3, "仿真天数（可为小数）")
	load := fs.Float64("load", 1.0, "流量负载倍率")
	seed := fs.Int64("seed", 20261123, "随机种子（保证可复现）")
	outDir := fs.String("out", "output", "报告输出目录")
	workers := fs.Int("workers", 0, "并行 worker 数（0=按 CPU 核数，1=串行）")
	cpuprof := fs.String("cpuprofile", "", "可选：写入 CPU profile 的路径")
	_ = fs.Parse(args)

	if err := report.EnsureDir(*outDir); err != nil {
		log.Fatal(err)
	}
	dur := time.Duration(*days * float64(24*time.Hour))
	cfg := sim.FluidConfig{
		Stores: *stores, Duration: dur, Seed: *seed,
		LoadMul: *load, SyncEvery: 10 * time.Second, Workers: *workers,
	}
	costPerGB := 8.0

	if *cpuprof != "" {
		f, _ := os.Create(*cpuprof)
		_ = pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	log.Printf("[基线] 全走专线：%d 门店 × %s ...", *stores, dur)
	t1 := time.Now()
	baseRun := sim.NewFluid(edge.ModeBaseline, cfg)
	faultLines := baseRun.Scenario().FaultSummary(baseRun.Now(), dur)
	base := baseRun.Run()
	log.Printf("[基线] 完成，用时 %s", time.Since(t1))

	log.Printf("[实验] 按质量选路：同样负载与故障场景 ...")
	t2 := time.Now()
	sd := sim.NewFluid(edge.ModeSDWAN, cfg).Run()
	log.Printf("[实验] 完成，用时 %s", time.Since(t2))

	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	cmp := report.Build(*stores, dur, start, costPerGB, base, sd)
	cmp.FaultLines = faultLines
	mdPath := *outDir + "/acceptance.md"
	jsonPath := *outDir + "/acceptance.json"
	if err := cmp.WriteMarkdown(mdPath); err != nil {
		log.Fatal(err)
	}
	if err := cmp.WriteJSON(jsonPath); err != nil {
		log.Fatal(err)
	}
	printSummary(cmp)
	log.Printf("报告已写入 %s 和 %s", mdPath, jsonPath)
}

func runQuick(args []string) {
	fs := flag.NewFlagSet("quick", flag.ExitOnError)
	stores := fs.Int("stores", 20, "门店数")
	hours := fs.Float64("hours", 6, "仿真小时数")
	_ = fs.Parse(args)

	dur := time.Duration(*hours * float64(time.Hour))
	cfg := sim.FluidConfig{
		Stores: *stores, Duration: dur, Seed: 42,
		LoadMul: 1, SyncEvery: 10 * time.Second,
	}
	baseRun := sim.NewFluid(edge.ModeBaseline, cfg)
	faultLines := baseRun.Scenario().FaultSummary(baseRun.Now(), dur)
	base := baseRun.Run()
	sd := sim.NewFluid(edge.ModeSDWAN, cfg).Run()
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	cmp := report.Build(*stores, dur, start, 8.0, base, sd)
	cmp.FaultLines = faultLines
	if err := report.EnsureDir("output"); err != nil {
		log.Fatal(err)
	}
	_ = cmp.WriteMarkdown("output/quick.md")
	_ = cmp.WriteJSON("output/quick.json")
	printSummary(cmp)
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "监听地址")
	_ = fs.Parse(args)

	demo := web.NewDemo()
	demo.Start()
	log.Printf("SD-WAN 仪表盘已启动：http://localhost%s （Ctrl-C 退出）", *addr)
	if err := http.ListenAndServe(*addr, demo.Handler()); err != nil {
		log.Fatal(err)
	}
}

func printSummary(c *report.Comparison) {
	fmt.Println("\n================ 验收摘要 ================")
	fmt.Printf("%-18s %12s %12s\n", "应用 SLA 达标率", "全走专线", "按质量选路")
	b, s := c.Baseline.Metrics.Snapshot(), c.SDWAN.Metrics.Snapshot()
	for _, app := range model.AllApps {
		ba := b.App[app]
		sa := s.App[app]
		fmt.Printf("%-18s %11.2f%% %11.2f%%\n", string(app),
			ba.SLARate()*100, sa.SLARate()*100)
	}
	fmt.Println("------------------------------------------")
	mplsReduction := 0.0
	if b.Link[model.LinkMPLS].BytesUp > 0 {
		mplsReduction = (1 - float64(s.Link[model.LinkMPLS].BytesUp)/
			float64(b.Link[model.LinkMPLS].BytesUp)) * 100
	}
	fmt.Printf("专线上行流量       %12s %12s  (%.1f%% ↓)\n",
		human(b.Link[model.LinkMPLS].BytesUp),
		human(s.Link[model.LinkMPLS].BytesUp), mplsReduction)
	fmt.Printf("移动网费用         %12s %12s\n",
		money(b, c.CostPerGB), money(s, c.CostPerGB))
	fmt.Printf("关键应用平均中断   %12s %12s\n",
		c.Baseline.Metrics.MeanCriticalInterrupt().Round(time.Second),
		c.SDWAN.Metrics.MeanCriticalInterrupt().Round(time.Second))
	fmt.Printf("关键应用最长中断   %12s %12s\n",
		c.Baseline.Metrics.MaxCriticalInterrupt().Round(time.Second),
		c.SDWAN.Metrics.MaxCriticalInterrupt().Round(time.Second))
	fmt.Printf("流迁移/反复抖动    %12s %12s\n",
		"-", fmt.Sprintf("%d / %d", s.Migrations, s.Flaps))
	fmt.Printf("识别准确率         %12s %11.2f%%\n", "-", c.SDWAN.Metrics.ClassRate()*100)
	fmt.Println("==========================================")
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func money(m *sim.Metrics, perGB float64) string {
	return fmt.Sprintf("¥%.2f", m.CellularCost(perGB))
}

func runRerender(args []string) {
	fs := flag.NewFlagSet("rerender", flag.ExitOnError)
	jsonPath := fs.String("json", "output/acceptance.json", "输入的紧凑验收 JSON")
	mdPath := fs.String("md", "output/acceptance.md", "输出的 Markdown 路径")
	_ = fs.Parse(args)
	if err := report.RerenderFromJSON(*jsonPath, *mdPath); err != nil {
		log.Fatal(err)
	}
	log.Printf("已从 %s 重新生成 %s", *jsonPath, *mdPath)
}
