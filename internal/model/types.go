// Package model defines the shared domain types of the SD-WAN system:
// applications, links, link-quality samples, policy rules and telemetry.
package model

import "time"

// AppID identifies an application class.
type AppID string

const (
	AppPOS     AppID = "pos"     // 收银/支付：交易类小包，对丢包和时延极敏感
	AppVC      AppID = "vc"      // 视频会议：实时 UDP，对丢包/抖动敏感
	AppBulk    AppID = "bulk"    // 备份/系统更新：大流量，可后台限速
	AppWeb     AppID = "web"     // 普通办公网页
	AppGeneral AppID = "general" // 识别不出的流量，按行为归类后的默认类
)

// AllApps lists application classes in priority order.
var AllApps = []AppID{AppPOS, AppVC, AppBulk, AppWeb, AppGeneral}

// NumApps is the fixed number of application classes for zero-alloc arrays.
const NumApps = 5

func (a AppID) Valid() bool {
	for _, x := range AllApps {
		if x == a {
			return true
		}
	}
	return false
}

// LinkType identifies a physical WAN link technology.
type LinkType string

const (
	LinkMPLS      LinkType = "mpls"      // 专线：贵、稳
	LinkBroadband LinkType = "broadband" // 宽带：便宜、晚间劣化
	LinkCellular  LinkType = "cellular"  // 移动网卡：按流量计费
)

var AllLinkTypes = []LinkType{LinkMPLS, LinkBroadband, LinkCellular}

// NumLinkTypes is the fixed number of link technologies; used to size
// zero-allocation per-link arrays in the forwarding hot path.
const NumLinkTypes = 3

// LinkIndex maps a link type to a stable 0..NumLinkTypes-1 index.
func LinkIndex(t LinkType) int {
	switch t {
	case LinkMPLS:
		return 0
	case LinkBroadband:
		return 1
	case LinkCellular:
		return 2
	}
	return 0
}

// LinkByIndex is the inverse of LinkIndex.
func LinkByIndex(i int) LinkType {
	if i >= 0 && i < len(AllLinkTypes) {
		return AllLinkTypes[i]
	}
	return LinkMPLS
}

// LinkID uniquely identifies a link of one edge device.
type LinkID struct {
	EdgeID string   `json:"edge_id"`
	Type   LinkType `json:"type"`
}

func (l LinkID) String() string { return l.EdgeID + "/" + string(l.Type) }

// Endpoint is an IP-like host address inside the simulated network.
type Endpoint struct {
	IP   string `json:"ip"`
	Port uint16 `json:"port"`
}

// FlowKey is the 5-tuple used to key a cached classification / forwarding state.
type FlowKey struct {
	Proto string   `json:"proto"` // "tcp" | "udp"
	Src   Endpoint `json:"src"`
	Dst   Endpoint `json:"dst"`
}

func (f FlowKey) String() string {
	return f.Proto + ":" + f.Src.IP + "->" + f.Dst.IP + ":" + itoa(f.Dst.Port)
}

func itoa(p uint16) string {
	if p == 0 {
		return "0"
	}
	var b [5]byte
	i := len(b)
	for p > 0 {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
	}
	return string(b[i:])
}

// Quality is a measured/observed link-quality sample (RTT based).
type Quality struct {
	RTT    float64 `json:"rtt_ms"`    // 往返时延（毫秒）
	Loss   float64 `json:"loss"`      // 丢包率 0..1
	Jitter float64 `json:"jitter_ms"` // 抖动（毫秒）
	// Up/Down capacity currently available (bps), observed from the link model
	// or estimated from delivery timing in packet mode.
	UpBps   float64 `json:"up_bps"`
	DownBps float64 `json:"down_bps"`
	// Utilization is the fraction of capacity currently in use 0..1.
	Util float64 `json:"util"`
	// Healthy is false when the link is down / completely unusable.
	Healthy bool `json:"healthy"`
}

// AppProfile describes an application's quality requirements and preferences.
type AppProfile struct {
	App AppID `json:"app"`
	// Hard SLA: a link meeting these is "eligible" for the app.
	MaxRTT    float64 `json:"max_rtt_ms"`
	MaxLoss   float64 `json:"max_loss"`
	MaxJitter float64 `json:"max_jitter_ms"`
	// Critical apps get dual-link replication when every link violates SLA.
	Critical bool `json:"critical"`
	// Realtime flows (RTP) migrate without reorder buffering; others use a
	// reorder window across migration.
	Realtime bool `json:"realtime"`
	// Bulk flows are rate-limited and restricted to cheap links.
	Bulk bool `json:"bulk"`
	// Preference order of link types for this app (best first).
	Preference []LinkType `json:"preference"`
	// Priority for queue scheduling / admission (higher = more important).
	Priority int `json:"priority"`
}

// DefaultProfiles returns the built-in application SLA/profile table.
func DefaultProfiles() map[AppID]*AppProfile {
	return map[AppID]*AppProfile{
		AppPOS: {
			App: AppPOS, MaxRTT: 120, MaxLoss: 0.01, MaxJitter: 30,
			Critical: true, Realtime: false,
			Preference: []LinkType{LinkMPLS, LinkCellular, LinkBroadband},
			Priority:   100,
		},
		AppVC: {
			App: AppVC, MaxRTT: 200, MaxLoss: 0.02, MaxJitter: 40,
			Critical: true, Realtime: true,
			Preference: []LinkType{LinkMPLS, LinkBroadband, LinkCellular},
			Priority:   90,
		},
		AppWeb: {
			App: AppWeb, MaxRTT: 400, MaxLoss: 0.03, MaxJitter: 100,
			Preference: []LinkType{LinkBroadband, LinkMPLS, LinkCellular},
			Priority:   50,
		},
		AppBulk: {
			App: AppBulk, MaxRTT: 1e9, MaxLoss: 0.1, MaxJitter: 1e9,
			Bulk:       true,
			Preference: []LinkType{LinkBroadband, LinkCellular, LinkMPLS},
			Priority:   10,
		},
		AppGeneral: {
			App: AppGeneral, MaxRTT: 500, MaxLoss: 0.05, MaxJitter: 150,
			Preference: []LinkType{LinkBroadband, LinkMPLS, LinkCellular},
			Priority:   30,
		},
	}
}

// PortRule matches flows by destination IP prefix and port range.
type PortRule struct {
	Name   string   `json:"name"`
	App    AppID    `json:"app"`
	Prefix string   `json:"dst_prefix"` // CIDR, e.g. "10.8.0.0/16"; "" = any
	Ports  []uint16 `json:"dst_ports"`  // exact ports
}

// SignatureRule matches on first-packet payload features.
type SignatureRule struct {
	Name      string `json:"name"`
	App       AppID  `json:"app"`
	PrefixHex string `json:"prefix_hex"` // payload must start with these bytes
	// Optional TLS SNI / DNS name marker embedded in the synthetic payload.
	NameMarker string `json:"name_marker,omitempty"`
}

// DNSRecord associates a resolved IP with the application behind a domain.
type DNSRecord struct {
	Domain string `json:"domain"`
	IP     string `json:"ip"`
	App    AppID  `json:"app"`
}

// Policy is the full controller-pushed configuration for an edge device.
type Policy struct {
	Version     int64                 `json:"version"`
	UpdatedAt   time.Time             `json:"updated_at"`
	Profiles    map[AppID]*AppProfile `json:"profiles"`
	PortRules   []PortRule            `json:"port_rules"`
	SigRules    []SignatureRule       `json:"signature_rules"`
	DNSMappings []DNSRecord           `json:"dns_mappings"`
	// Per-link cost per GB (cellular typically non-zero).
	CostPerGB map[LinkType]float64 `json:"cost_per_gb"`
	// Bulk shaping: per-edge aggregate cap for bulk traffic, bps.
	BulkCapBps float64 `json:"bulk_cap_bps"`
	// Hysteresis: a candidate link must beat the current one by this score
	// margin, and the current link must stay bad for HoldDown before migration.
	MigrateScoreMargin float64       `json:"migrate_score_margin"`
	HoldDown           time.Duration `json:"hold_down"`
	// Probe interval.
	ProbeInterval time.Duration `json:"probe_interval"`
	// Reorder window tolerated at the receiver when migrating non-realtime flows.
	ReorderWindow time.Duration `json:"reorder_window"`
}

// DefaultPolicy builds the baseline policy used before the controller is seen.
func DefaultPolicy() *Policy {
	return &Policy{
		Version:            1,
		UpdatedAt:          time.Now(),
		Profiles:           DefaultProfiles(),
		CostPerGB:          map[LinkType]float64{LinkMPLS: 0, LinkBroadband: 0, LinkCellular: 8.0},
		BulkCapBps:         4_000_000,
		MigrateScoreMargin: 12,
		HoldDown:           20 * time.Second,
		ProbeInterval:      time.Second,
		ReorderWindow:      250 * time.Millisecond,
		PortRules:          DefaultPortRules(),
		SigRules:           DefaultSignatureRules(),
		DNSMappings:        DefaultDNSMappings(),
	}
}

// DefaultPortRules are the destination-based classification rules.
func DefaultPortRules() []PortRule {
	return []PortRule{
		{Name: "pos-hq", App: AppPOS, Prefix: "10.8.0.0/16", Ports: []uint16{443, 8443}},
		{Name: "pos-pay", App: AppPOS, Prefix: "100.64.10.0/24", Ports: []uint16{7000, 7001}},
		{Name: "vc-media", App: AppVC, Prefix: "10.9.0.0/16", Ports: []uint16{5004, 5005}},
		{Name: "vc-signal", App: AppVC, Prefix: "10.9.0.0/16", Ports: []uint16{5060, 5061}},
		{Name: "backup-dc", App: AppBulk, Prefix: "10.10.0.0/16", Ports: []uint16{8443, 8444}},
		{Name: "update-cdn", App: AppBulk, Prefix: "10.10.2.0/24", Ports: []uint16{80, 443}},
		// 办公网段的网页/HTTPS 流量
		{Name: "office-web", App: AppWeb, Prefix: "172.16.0.0/12", Ports: []uint16{80, 443, 8080}},
	}
}

// DefaultSignatureRules are first-packet payload signatures.
func DefaultSignatureRules() []SignatureRule {
	return []SignatureRule{
		{Name: "pos-iso8583", App: AppPOS, PrefixHex: "49534f38353833"}, // "ISO8583"
		{Name: "rtp", App: AppVC, PrefixHex: "80"},                      // RTP v2 (version field top bits)
		{Name: "tls-sni-meet", App: AppVC, PrefixHex: "160303", NameMarker: "meet"},
		{Name: "tls-sni-pos", App: AppPOS, PrefixHex: "160303", NameMarker: "pay"},
		{Name: "rsync", App: AppBulk, PrefixHex: "405253594e43"}, // "@RSYNC"
	}
}

// DefaultDNSMappings ties observed DNS resolutions to applications.
func DefaultDNSMappings() []DNSRecord {
	return []DNSRecord{
		{Domain: "pay.example.com", IP: "100.64.10.7", App: AppPOS},
		// 仅 DNS 能识别的地址：端口规则不覆盖 9443
		{Domain: "pay2.example.com", IP: "100.64.10.8", App: AppPOS},
		{Domain: "meet.example.com", IP: "10.9.0.20", App: AppVC},
		{Domain: "backup.example.com", IP: "10.10.0.5", App: AppBulk},
		{Domain: "update.example.com", IP: "10.10.2.9", App: AppBulk},
	}
}

// FlowState is the per-flow entry cached on the edge until the flow ends.
type FlowState struct {
	Key       FlowKey   `json:"key"`
	App       AppID     `json:"app"`
	ClassWhy  string    `json:"class_why"` // 哪条规则/哪个阶段识别的
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
	Active    bool      `json:"active"`
	// Current primary link and (optionally) replicated secondary link.
	Primary LinkType `json:"primary"`
	Replica LinkType `json:"replica,omitempty"`
	// Migration bookkeeping.
	Since      time.Time `json:"since"`
	BadSince   time.Time `json:"bad_since"`
	Migrating  bool      `json:"migrating"`
	ReorderSeq uint64    `json:"reorder_seq"`
	BytesSent  int64     `json:"bytes_sent"`
	// Ground-truth app from the generator (empty in real deployments); used by
	// the acceptance harness to score classifier accuracy.
	Truth AppID `json:"truth,omitempty"`
}

// FlowEvent records a link-selection / migration / replication decision.
type FlowEvent struct {
	At      time.Time `json:"at"`
	EdgeID  string    `json:"edge_id"`
	Flow    FlowKey   `json:"flow"`
	App     AppID     `json:"app"`
	From    LinkType  `json:"from,omitempty"`
	To      LinkType  `json:"to"`
	Replica LinkType  `json:"replica,omitempty"`
	Reason  string    `json:"reason"`
}

// EdgeTelemetry is the per-edge snapshot reported to the controller.
type EdgeTelemetry struct {
	EdgeID       string                     `json:"edge_id"`
	At           time.Time                  `json:"at"`
	Links        map[LinkType]LinkTelemetry `json:"links"`
	Flows        []FlowTelemetry            `json:"flows"`
	PolicyVer    int64                      `json:"policy_version"`
	ControllerUp bool                       `json:"controller_up"`
	Events       []FlowEvent                `json:"events,omitempty"`
}

// LinkTelemetry is one link's observed quality and usage in a snapshot.
type LinkTelemetry struct {
	Type     LinkType `json:"type"`
	Quality  Quality  `json:"quality"`
	BpsInUse float64  `json:"bps_in_use"`
	BytesUp  int64    `json:"bytes_up"`
	Cost     float64  `json:"cost"`
	Outage   bool     `json:"outage"`
}

// FlowTelemetry is a compact per-flow view in a snapshot.
type FlowTelemetry struct {
	Key      FlowKey  `json:"key"`
	App      AppID    `json:"app"`
	Primary  LinkType `json:"primary"`
	Replica  LinkType `json:"replica,omitempty"`
	Bps      float64  `json:"bps"`
	ClassWhy string   `json:"class_why"`
}

// StoreInfo describes a simulated store.
type StoreInfo struct {
	ID      string     `json:"id"`
	Region  int        `json:"region"`
	Subnet  string     `json:"subnet"`
	HasCell bool       `json:"has_cell"`
	Links   []LinkType `json:"links"`
}
