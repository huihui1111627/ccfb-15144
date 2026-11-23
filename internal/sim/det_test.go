package sim

import (
	"testing"
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
)

func TestDeterminismAcrossWorkerCounts(t *testing.T) {
	cfg := FluidConfig{Stores: 12, Duration: 20 * time.Minute, Seed: 99,
		LoadMul: 1, SyncEvery: 10 * time.Second}
	ref := NewFluid(edge.ModeSDWAN, cfg)
	ref.workers = 1
	ref.Run()
	s1 := ref.Metrics().Snapshot()

	par := NewFluid(edge.ModeSDWAN, cfg)
	par.workers = 8
	par.Run()
	s2 := par.Metrics().Snapshot()

	for _, a := range model.AllApps {
		if s1.App[a].SLAMet != s2.App[a].SLAMet || s1.App[a].Samples != s2.App[a].Samples {
			t.Fatalf("app %s differs: serial met=%d/%d parallel met=%d/%d", a,
				s1.App[a].SLAMet, s1.App[a].Samples, s2.App[a].SLAMet, s2.App[a].Samples)
		}
	}
	for _, lk := range model.AllLinkTypes {
		if s1.Link[lk].BytesUp != s2.Link[lk].BytesUp {
			t.Fatalf("link %s bytes differ: %d vs %d", lk,
				s1.Link[lk].BytesUp, s2.Link[lk].BytesUp)
		}
	}
	if s1.ClassCorrect != s2.ClassCorrect {
		t.Fatalf("classification differs %d vs %d", s1.ClassCorrect, s2.ClassCorrect)
	}
}
