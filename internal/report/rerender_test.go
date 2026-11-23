package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/sim"
)

func TestRerenderFromJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "acceptance.json")
	mdPath := filepath.Join(dir, "acceptance.md")

	compact := struct {
		GeneratedAt     time.Time          `json:"generated_at"`
		SimStart        time.Time          `json:"sim_start"`
		Duration        time.Duration      `json:"duration"`
		Stores          int                `json:"stores"`
		CostPerGB       float64            `json:"cellular_cost_per_gb"`
		FaultLines      []string           `json:"injected_faults"`
		BaselineMetrics sim.CompactMetrics `json:"baseline_metrics"`
		SDWANMetrics    sim.CompactMetrics `json:"sdwan_metrics"`
	}{
		GeneratedAt: time.Now(), SimStart: time.Now(), Duration: 24 * time.Hour,
		Stores: 50, CostPerGB: 8,
		FaultLines: []string{"第 1 天 10:00，全网门店，专线拥塞"},
		BaselineMetrics: sim.CompactMetrics{
			App: map[model.AppID]*sim.AppMetrics{
				model.AppPOS: {App: model.AppPOS, Samples: 100, SLAMet: 80},
			},
			Link: map[model.LinkType]*sim.LinkMetrics{
				model.LinkMPLS: {BytesUp: 1000},
			},
			InterruptCount: 10, InterruptMeanMs: 5000, InterruptMaxMs: 9000,
		},
		SDWANMetrics: sim.CompactMetrics{
			App: map[model.AppID]*sim.AppMetrics{
				model.AppPOS: {App: model.AppPOS, Samples: 100, SLAMet: 95},
			},
			Link: map[model.LinkType]*sim.LinkMetrics{
				model.LinkMPLS:      {BytesUp: 200},
				model.LinkBroadband: {BytesUp: 800},
				model.LinkCellular:  {BytesUp: 50},
			},
			InterruptCount: 4, InterruptMeanMs: 2000, InterruptMaxMs: 3000,
			Migrations: 100, Flaps: 5, Replications: 10,
			ClassSamples: 200, ClassCorrect: 200, AutonomousTicks: 60,
		},
	}
	writeTestJSON(t, jsonPath, compact)

	if err := RerenderFromJSON(jsonPath, mdPath); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(md)
	for _, want := range []string{
		"收银 POS", "80.00%", "95.00%",
		"乒乓抖动 5 次", "控制器失联期间自治 tick：60",
		"专线拥塞",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("markdown missing %q", want)
		}
	}
}

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
