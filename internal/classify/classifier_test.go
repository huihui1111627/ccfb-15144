package classify

import (
	"testing"
	"time"

	"sdwan/internal/model"
)

func t0() time.Time { return time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC) }

func flowKey(dstIP string, port uint16) model.FlowKey {
	return model.FlowKey{
		Proto: "tcp",
		Src:   model.Endpoint{IP: "192.168.1.20", Port: 40000},
		Dst:   model.Endpoint{IP: dstIP, Port: port},
	}
}

func TestPortRuleClassification(t *testing.T) {
	c := New(model.DefaultPolicy())
	// 10.8.0.0/16:443 -> pos
	app, why := c.Classify(Info{Key: flowKey("10.8.0.11", 443), First: true}, t0())
	if app != model.AppPOS || why != "port:pos-hq" {
		t.Fatalf("got %s/%s", app, why)
	}
	// 10.9/16:5004 -> vc
	app, _ = c.Classify(Info{Key: flowKey("10.9.0.20", 5004), First: true}, t0())
	if app != model.AppVC {
		t.Fatalf("got %s", app)
	}
}

func TestDNSAssociation(t *testing.T) {
	c := New(model.DefaultPolicy())
	// pay2 解析地址，端口 9443 无规则覆盖，只能靠 DNS
	c.ObserveDNS("pay2.example.com", "100.64.10.8")
	app, why := c.Classify(Info{Key: flowKey("100.64.10.8", 9443), First: true,
		Payload: []byte{0x16, 0x03, 0x03}}, t0())
	if app != model.AppPOS || why != "dns:pay2.example.com" {
		t.Fatalf("got %s/%s", app, why)
	}
}

func TestSignatureClassification(t *testing.T) {
	c := New(model.DefaultPolicy())
	// 指向未知地址段，只有首包签名能识别
	k := flowKey("203.0.113.9", 443)
	app, why := c.Classify(Info{Key: k, First: true,
		Payload: []byte("ISO8583-0200")}, t0())
	if app != model.AppPOS || why != "sig:pos-iso8583" {
		t.Fatalf("got %s/%s", app, why)
	}
	// RTP
	k2 := model.FlowKey{Proto: "udp", Dst: model.Endpoint{IP: "203.0.113.9", Port: 9000}}
	app, why = c.Classify(Info{Key: k2, First: true,
		Payload: []byte{0x80, 0x60, 0x00, 0x01, 'm', 'e', 'e', 't'}}, t0())
	if app != model.AppVC || why != "sig:rtp" {
		t.Fatalf("got %s/%s", app, why)
	}
}

func TestVerdictCachedUntilFlowEnds(t *testing.T) {
	c := New(model.DefaultPolicy())
	k := flowKey("10.8.0.11", 443)
	c.Classify(Info{Key: k, First: true}, t0())
	_, _ = c.Classify(Info{Key: k, First: false}, t0().Add(time.Second))
	s := c.Stats()
	if s.CacheHits != 1 {
		t.Fatalf("cache hits = %d", s.CacheHits)
	}
	c.End(k)
	// 流结束后缓存应删除，下次是全新识别
	c.Classify(Info{Key: k, First: true}, t0().Add(2*time.Second))
	s = c.Stats()
	if s.PortRuleHits != 2 {
		t.Fatalf("port hits after end = %d, want 2", s.PortRuleHits)
	}
}

func TestBehavioralFallback(t *testing.T) {
	c := New(model.DefaultPolicy())
	k := flowKey("198.51.100.55", 7777)
	app, why := c.Classify(Info{Key: k, First: true, Payload: []byte{0xaa, 0xbb}}, t0())
	if app != model.AppGeneral || why != "pending" {
		t.Fatalf("initial = %s/%s", app, why)
	}
	// 大包大流量 -> bulk
	promoted := false
	for i := 0; i < 5; i++ {
		if a, ok := c.ObserveBehavior(k, BehaviorObs{
			Packets: 10, Bytes: 300_000, MeanPkt: 1300, IntervalS: 0.01,
		}, t0().Add(time.Duration(i+1)*time.Second)); ok {
			if a != model.AppBulk {
				t.Fatalf("behavior class = %s, want bulk", a)
			}
			promoted = true
			break
		}
	}
	if !promoted {
		t.Fatal("behavior classification never promoted")
	}
}

func TestBehaviorPeriodicSmall(t *testing.T) {
	c := New(model.DefaultPolicy())
	k := model.FlowKey{Proto: "udp", Dst: model.Endpoint{IP: "198.51.100.9", Port: 6000}}
	c.Classify(Info{Key: k, First: true, Payload: []byte{0x01}}, t0())
	for i := 0; i < 5; i++ {
		if a, ok := c.ObserveBehavior(k, BehaviorObs{
			Packets: 10, Bytes: 2000, MeanPkt: 150, IntervalS: 0.02,
		}, t0().Add(time.Duration(i+1)*time.Second)); ok {
			if a != model.AppVC {
				t.Fatalf("got %s, want vc", a)
			}
			return
		}
	}
	t.Fatal("periodic-small flow not classified as vc")
}
