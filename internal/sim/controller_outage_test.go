package sim

import (
	"testing"
	"time"

	"sdwan/internal/edge"
)

func TestControllerOutageAutonomy(t *testing.T) {
	// 场景：第 1 分钟后控制器整体失联 10 分钟
	t0 := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	cfg := FluidConfig{Stores: 4, Duration: 12 * time.Minute, Seed: 3,
		LoadMul: 1, SyncEvery: 10 * time.Second}
	f := NewFluid(edge.ModeSDWAN, cfg)
	f.scen = &Scenario{Faults: []Fault{{
		Kind: FaultControllerDown, Start: t0.Add(time.Minute), Duration: 10 * time.Minute,
	}}}
	f.fr = newFaultRuntime(f.scen)

	// 手动驱动，统计失联窗口内的自治 tick 与转发是否持续
	var autonomousTicks, forwardedTicks int64
	ticks := int(cfg.Duration / time.Second)
	for i := 0; i < ticks; i++ {
		f.Step()
		for _, rt := range f.runtimes {
			if !rt.eng.ControllerUp() {
				autonomousTicks++
			}
			// 失联期间仍有流被分配到链路，说明用最后策略继续工作
			if f.now.After(t0.Add(70*time.Second)) && f.now.Before(t0.Add(10*time.Minute)) {
				for _, fl := range rt.eng.Flows() {
					if fl.Primary != "" {
						forwardedTicks++
						break
					}
				}
			}
		}
	}
	if autonomousTicks == 0 {
		t.Fatal("no autonomous ticks recorded during controller outage")
	}
	if forwardedTicks == 0 {
		t.Fatal("edge stopped forwarding while controller was unreachable")
	}
	// 失联期间策略版本保持不变（最后一份策略）
	for _, rt := range f.runtimes {
		if rt.eng.Policy().Version == 0 {
			t.Fatal("edge lost its policy during controller outage")
		}
	}
}

func TestControllerReconnectResumes(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	cfg := FluidConfig{Stores: 3, Duration: 6 * time.Minute, Seed: 5,
		LoadMul: 1, SyncEvery: 5 * time.Second}
	f := NewFluid(edge.ModeSDWAN, cfg)
	// 失联 2 分钟后恢复
	f.scen = &Scenario{Faults: []Fault{{
		Kind: FaultControllerDown, Start: t0.Add(30 * time.Second), Duration: 2 * time.Minute,
	}}}
	f.fr = newFaultRuntime(f.scen)

	ticks := int(cfg.Duration / time.Second)
	sawDown, sawUpAfter := false, false
	for i := 0; i < ticks; i++ {
		f.Step()
		down := false
		for _, rt := range f.runtimes {
			if !rt.eng.ControllerUp() {
				down = true
			}
		}
		if down {
			sawDown = true
		}
		if sawDown && !down && f.now.After(t0.Add(3*time.Minute)) {
			sawUpAfter = true
		}
	}
	if !sawDown {
		t.Fatal("controller outage never observed by edges")
	}
	if !sawUpAfter {
		t.Fatal("edges did not reconnect after controller recovery")
	}
}
