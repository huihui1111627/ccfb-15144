// Package classify implements application-aware traffic identification:
//
//  1. destination prefix + port rules
//  2. DNS resolution association (domain -> resolved IP -> app)
//  3. first-packet payload signatures (ISO8583, RTP, TLS SNI marker, rsync)
//  4. behavioral fallback for anything unmatched (packet-size / rate /
//     periodicity heuristics), with the verdict cached per flow until it ends.
package classify

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"sync"
	"time"

	"sdwan/internal/model"
)

// BehaviorObs is the per-flow observation feeding behavioral classification.
type BehaviorObs struct {
	Packets   int
	Bytes     int64
	MeanPkt   float64 // 平均包长
	IntervalS float64 // 平均包间隔（秒），0 表示未知
}

type cacheEntry struct {
	app     model.AppID
	why     string
	seen    time.Time
	pending bool // 尚未最终识别，等待行为观察
	obs     BehaviorObs
	created time.Time
}

// Stats exposes classifier effectiveness counters.
type Stats struct {
	Lookups      int64
	CacheHits    int64
	PortRuleHits int64
	DNSHits      int64
	SigHits      int64
	BehaviorHits int64
	Unknown      int64
	Evictions    int64
}

// Classifier holds rules, the dynamic DNS cache and the per-flow verdict cache.
type Classifier struct {
	mu       sync.Mutex
	policy   *model.Policy
	prefixes []parsedPortRule
	sigRules []parsedSigRule
	dnsIP    map[string]model.DNSRecord // resolved IP -> record
	cache    map[model.FlowKey]*cacheEntry
	maxFlows int
	idleTTL  time.Duration
	stats    Stats
}

type parsedPortRule struct {
	model.PortRule
	prefix *netip.Prefix // nil = any
	ports  map[uint16]struct{}
}

type parsedSigRule struct {
	model.SignatureRule
	prefix []byte
}

// New builds a classifier from a policy.
func New(policy *model.Policy) *Classifier {
	c := &Classifier{
		policy:   policy,
		dnsIP:    map[string]model.DNSRecord{},
		cache:    map[model.FlowKey]*cacheEntry{},
		maxFlows: 200_000,
		idleTTL:  2 * time.Minute,
	}
	c.compile()
	for _, r := range policy.DNSMappings {
		c.dnsIP[r.IP] = r
	}
	return c
}

func (c *Classifier) compile() {
	c.prefixes = nil
	for _, r := range c.policy.PortRules {
		pr := parsedPortRule{PortRule: r, ports: map[uint16]struct{}{}}
		if r.Prefix != "" {
			p, err := netip.ParsePrefix(r.Prefix)
			if err == nil {
				pr.prefix = &p
			}
		}
		for _, p := range r.Ports {
			pr.ports[p] = struct{}{}
		}
		c.prefixes = append(c.prefixes, pr)
	}
	c.sigRules = nil
	for _, r := range c.policy.SigRules {
		raw, err := hex.DecodeString(r.PrefixHex)
		if err != nil {
			continue
		}
		c.sigRules = append(c.sigRules, parsedSigRule{SignatureRule: r, prefix: raw})
	}
}

// UpdatePolicy hot-swaps the rule set while keeping cached verdicts and DNS.
func (c *Classifier) UpdatePolicy(p *model.Policy) {
	c.mu.Lock()
	c.policy = p
	c.compile()
	c.mu.Unlock()
}

// ObserveDNS records a simulated DNS response (domain resolved to ip).
// Domains present in the policy map get associated; unknown domains are ignored.
func (c *Classifier) ObserveDNS(domain, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.policy.DNSMappings {
		if m.Domain == domain {
			c.dnsIP[ip] = m
			return
		}
	}
}

// Info is what the edge passes for each first packet / flow lookup.
type Info struct {
	Key     model.FlowKey
	First   bool
	Payload []byte
}

// Classify returns the cached verdict for a flow, identifying it on first
// sight. Unmatched flows stay "pending" until ObserveBehavior promotes them.
func (c *Classifier) Classify(in Info, now time.Time) (model.AppID, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Lookups++
	if e, ok := c.cache[in.Key]; ok {
		e.seen = now
		c.stats.CacheHits++
		return e.app, e.why
	}

	// 1) 目的地址 + 端口规则
	if app, why, ok := c.matchPort(in.Key); ok {
		c.stats.PortRuleHits++
		c.put(in.Key, app, why, now, false)
		return app, why
	}
	// 2) DNS 解析记录关联
	if rec, ok := c.dnsIP[in.Key.Dst.IP]; ok {
		c.stats.DNSHits++
		why := "dns:" + rec.Domain
		c.put(in.Key, rec.App, why, now, false)
		return rec.App, why
	}
	// 3) 首包特征签名
	if in.First && len(in.Payload) > 0 {
		if app, why, ok := c.matchSig(in.Payload); ok {
			c.stats.SigHits++
			c.put(in.Key, app, why, now, false)
			return app, why
		}
	}
	// 4) 识别不了：挂起，等行为观察归类
	c.stats.Unknown++
	c.put(in.Key, model.AppGeneral, "pending", now, true)
	return model.AppGeneral, "pending"
}

func (c *Classifier) matchPort(k model.FlowKey) (model.AppID, string, bool) {
	addr, err := netip.ParseAddr(k.Dst.IP)
	for _, r := range c.prefixes {
		if _, ok := r.ports[k.Dst.Port]; !ok {
			continue
		}
		if r.prefix != nil {
			if err != nil || !r.prefix.Contains(addr) {
				continue
			}
		}
		return r.App, "port:" + r.Name, true
	}
	return "", "", false
}

func (c *Classifier) matchSig(payload []byte) (model.AppID, string, bool) {
	for _, r := range c.sigRules {
		if !bytes.HasPrefix(payload, r.prefix) {
			continue
		}
		if r.NameMarker != "" && !bytes.Contains(payload, []byte(r.NameMarker)) {
			continue
		}
		return r.App, "sig:" + r.Name, true
	}
	return "", "", false
}

func (c *Classifier) put(k model.FlowKey, app model.AppID, why string, now time.Time, pending bool) {
	if len(c.cache) >= c.maxFlows {
		c.evictLocked(now)
	}
	c.cache[k] = &cacheEntry{app: app, why: why, seen: now, pending: pending, created: now}
}

// ObserveBehavior feeds traffic statistics for a flow; a still-pending flow is
// classified by behavior once enough evidence accumulates.
func (c *Classifier) ObserveBehavior(k model.FlowKey, obs BehaviorObs, now time.Time) (model.AppID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[k]
	if !ok || !e.pending {
		return "", false
	}
	// 增量合并
	n := float64(obs.Packets)
	if n > 0 {
		e.obs.MeanPkt = (e.obs.MeanPkt*float64(e.obs.Packets) + obs.MeanPkt*n) / float64(e.obs.Packets+obs.Packets)
		if obs.IntervalS > 0 {
			e.obs.IntervalS = obs.IntervalS
		}
		e.obs.Packets += obs.Packets
		e.obs.Bytes += obs.Bytes
	}
	e.seen = now
	if e.obs.Packets < 20 {
		return "", false // 证据不足
	}
	app, why := behaviorClass(e.obs)
	e.pending = false
	e.app = app
	e.why = why
	c.stats.BehaviorHits++
	return app, true
}

// behaviorClass implements the fallback heuristics:
//   - 长连接、大包、高速率     -> bulk（备份/更新类大流量）
//   - 小包、稳定周期间隔(<=50ms) -> vc（实时媒体）
//   - 小包、稀疏请求-响应(>=100ms) -> pos（交易类）
//   - 其余                    -> general
func behaviorClass(o BehaviorObs) (model.AppID, string) {
	switch {
	case o.MeanPkt >= 900 && o.Bytes > 200_000:
		return model.AppBulk, "behavior:bulk-like"
	case o.MeanPkt <= 220 && o.IntervalS > 0 && o.IntervalS <= 0.05:
		return model.AppVC, "behavior:periodic-small"
	case o.MeanPkt <= 300 && o.IntervalS >= 0.1:
		return model.AppPOS, "behavior:tx-burst"
	default:
		return model.AppGeneral, "behavior:default"
	}
}

// End removes a flow's cached verdict when the flow terminates (requirement:
// the classification result is cached for the flow's lifetime only).
func (c *Classifier) End(k model.FlowKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, k)
}

// ReapIdle evicts idle entries; called periodically.
func (c *Classifier) ReapIdle(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictLocked(now)
}

func (c *Classifier) evictLocked(now time.Time) int {
	n := 0
	for k, e := range c.cache {
		if now.Sub(e.seen) > c.idleTTL {
			delete(c.cache, k)
			n++
		}
	}
	c.stats.Evictions += int64(n)
	return n
}

// Cached returns a snapshot copy of the flow verdict cache.
func (c *Classifier) Cached() map[model.FlowKey]model.FlowState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[model.FlowKey]model.FlowState, len(c.cache))
	for k, e := range c.cache {
		out[k] = model.FlowState{Key: k, App: e.app, ClassWhy: e.why, Active: !e.pending, LastSeen: e.seen}
	}
	return out
}

// Stats returns a copy of the effectiveness counters.
func (c *Classifier) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}
