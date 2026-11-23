package probe

import (
	"testing"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/network"
)

func testLink(t model.LinkType, rtt float64, loss float64) *network.LinkModel {
	return network.NewLink(network.LinkConfig{
		ID:       model.LinkID{EdgeID: "s1", Type: t},
		Type:     t,
		BaseRTT:  rtt,
		BaseJit:  5,
		BaseLoss: loss,
		UpBps:    10_000_000,
		DownBps:  20_000_000,
		Seed:     42,
	})
}

func TestTrackerMeasuresRTTAndLoss(t *testing.T) {
	link := testLink(model.LinkMPLS, 20, 0)
	tr := NewTracker(link)
	now := time.Now()
	// 30 个探测全部成功回包
	for i := 0; i < 30; i++ {
		now = now.Add(time.Second)
		tr.SyntheticTick(now)
	}
	q := tr.Quality()
	if !q.Healthy {
		t.Fatal("link should be healthy")
	}
	if q.Loss != 0 {
		t.Fatalf("loss = %v, want 0", q.Loss)
	}
	if q.RTT < 10 || q.RTT > 60 {
		t.Fatalf("rtt = %v outside plausible range", q.RTT)
	}
	if q.Jitter < 0 {
		t.Fatal("negative jitter")
	}
}

func TestTrackerDetectsOutage(t *testing.T) {
	link := testLink(model.LinkBroadband, 40, 0)
	tr := NewTracker(link)
	now := time.Now()
	for i := 0; i < 10; i++ {
		now = now.Add(time.Second)
		tr.SyntheticTick(now)
	}
	link.SetOutage(true)
	for i := 0; i < lossWindow; i++ {
		now = now.Add(time.Second)
		tr.SyntheticTick(now)
	}
	q := tr.Quality()
	if q.Healthy {
		t.Fatal("outage link must report unhealthy")
	}
	if q.Loss < 0.99 {
		t.Fatalf("loss = %v, want ~1.0", q.Loss)
	}
	// 恢复
	link.SetOutage(false)
	for i := 0; i < lossWindow; i++ {
		now = now.Add(time.Second)
		tr.SyntheticTick(now)
	}
	if !tr.Quality().Healthy {
		t.Fatal("link should recover")
	}
}

func TestProbeAckRTT(t *testing.T) {
	link := testLink(model.LinkCellular, 50, 0)
	tr := NewTracker(link)
	sent := time.Now()
	seq := tr.SendProbe(sent)
	tr.RecvProbeAck(seq, sent, sent.Add(55*time.Millisecond))
	q := tr.Quality()
	if q.RTT < 50 || q.RTT > 60 {
		t.Fatalf("rtt = %v, want ~55ms", q.RTT)
	}
}
