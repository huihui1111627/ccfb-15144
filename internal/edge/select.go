package edge

import (
	"time"

	"sdwan/internal/model"
)

// linkScore is the normalized 0..100 routing score of one link for one app.
type linkScore struct {
	link     model.LinkType
	score    float64
	eligible bool // 满足该应用硬 SLA
	quality  model.Quality
}

// scoreBoard is a fixed-size set of per-link scores, indexed by model.LinkIndex.
// It avoids slice/map allocations in the per-flow hot path.
type scoreBoard struct {
	sc [model.NumLinkTypes]linkScore
	n  int
}

func (b *scoreBoard) reset() { b.n = 0 }

func (b *scoreBoard) add(s linkScore) {
	if b.n < model.NumLinkTypes {
		b.sc[b.n] = s
		b.n++
	}
}

// scoreLinks evaluates every local link for an application. Higher is better.
// Factors: measured RTT / loss / jitter against the profile, utilization,
// monetary cost and the application's link preference.
func (e *Engine) scoreLinks(app model.AppID, b *scoreBoard) {
	b.reset()
	prof := e.policy.Profiles[app]
	if prof == nil {
		prof = e.policy.Profiles[model.AppGeneral]
	}
	for _, t := range e.order {
		tr := e.trackers[t]
		q := tr.Quality()
		ls := linkScore{link: t, quality: q}
		if !q.Healthy {
			ls.score = -1
			b.add(ls)
			continue
		}
		// 归一化子分（越接近要求上限越差）
		rttScore := clamp1(1 - q.RTT/prof.MaxRTT)
		lossScore := clamp1(1 - q.Loss/prof.MaxLoss)
		jitScore := clamp1(1 - q.Jitter/prof.MaxJitter)
		loadScore := 1 - clamp01(q.Util)
		// 费用因子：移动网最贵
		costScore := 1.0
		if e.policy.CostPerGB[t] > 0 {
			costScore = 0.35
		}
		// 偏好排序加成
		pref := 0.0
		for i, p := range prof.Preference {
			if p == t {
				pref = float64(len(prof.Preference)-i) / float64(len(prof.Preference))
				break
			}
		}
		ls.score = 100 * (0.30*rttScore + 0.30*lossScore + 0.15*jitScore +
			0.10*loadScore + 0.05*costScore + 0.10*pref)
		ls.eligible = q.RTT <= prof.MaxRTT && q.Loss <= prof.MaxLoss && q.Jitter <= prof.MaxJitter
		b.add(ls)
	}
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func clamp1(x float64) float64 {
	if x < 0.02 {
		return 0.02 // 不彻底归零，保证还有区分度
	}
	if x > 1 {
		return 1
	}
	return x
}

// Decision is the routing verdict for a flow at a tick. The scoreBoard is
// passed alongside (not embedded) so a Decision carries no per-flow array.
type Decision struct {
	Primary model.LinkType
	Replica model.LinkType // 0 表示不复制
	Reason  string
}

// chooseLink runs the full selection logic for one flow, reusing the engine's
// scratch scoreBoard so it performs zero heap allocations.
func (e *Engine) chooseLink(f *model.FlowState) Decision {
	prof := e.policy.Profiles[f.App]
	b := &e.scratch
	e.scoreLinks(f.App, b)

	// 基线模式：永远专线
	if e.mode == ModeBaseline {
		if _, ok := b.findType(model.LinkMPLS); ok {
			return Decision{Primary: model.LinkMPLS, Reason: "baseline-mpls"}
		}
		if best, ok := b.bestAll(); ok {
			return Decision{Primary: best.link, Reason: "baseline-fallback"}
		}
		return Decision{Reason: "no-link"}
	}

	bulk := prof != nil && prof.Bulk

	// 在候选池内挑合格/不合格的最优
	var eligible, violating linkScore
	hasElig, hasViol := false, false
	for i := 0; i < b.n; i++ {
		s := b.sc[i]
		if s.score < 0 {
			continue
		}
		// 大流量只走便宜链路：宽带优先、移动网次之、专线仅兜底
		if bulk && !cheapType(s.link) {
			if b.hasHealthy(model.LinkBroadband) || b.hasHealthy(model.LinkCellular) {
				continue
			}
		}
		if s.eligible {
			if !hasElig || s.score > eligible.score {
				eligible, hasElig = s, true
			}
		} else {
			if !hasViol || s.score > violating.score {
				violating, hasViol = s, true
			}
		}
	}

	if !hasElig {
		// 没有满足 SLA 的链路：取劣中最优
		if !hasViol {
			return Decision{Reason: "all-links-down"}
		}
		best := violating
		healthyCount := b.healthyCount()
		// 关键应用 + 至少两条可用 -> 双链路复制发送
		if prof != nil && prof.Critical && healthyCount >= 2 {
			second := secondBest(b, best.link, false)
			if second.score >= 0 {
				return Decision{Primary: best.link, Replica: second.link,
					Reason: "dual-replication-no-eligible-link"}
			}
		}
		return Decision{Primary: best.link, Reason: "best-effort-sla-violation"}
	}

	best := eligible
	// 关键应用且首选链路处于临界（分数很低但勉强合格）时，用次优链路做冗余
	if prof != nil && prof.Critical && best.score < 45 {
		second := secondBest(b, best.link, true)
		if second.score >= 30 {
			return Decision{Primary: best.link, Replica: second.link,
				Reason: "dual-replication-marginal-quality"}
		}
	}
	return Decision{Primary: best.link, Reason: "sla-best-score"}
}

func cheapType(t model.LinkType) bool {
	return t == model.LinkBroadband || t == model.LinkCellular
}

// findType returns the score entry for a link type.
func (b *scoreBoard) findType(t model.LinkType) (linkScore, bool) {
	for i := 0; i < b.n; i++ {
		if b.sc[i].link == t {
			return b.sc[i], true
		}
	}
	return linkScore{}, false
}

// bestAll returns the highest-scoring entry regardless of eligibility.
func (b *scoreBoard) bestAll() (linkScore, bool) {
	best := linkScore{score: -2}
	found := false
	for i := 0; i < b.n; i++ {
		if b.sc[i].score > best.score {
			best, found = b.sc[i], true
		}
	}
	return best, found
}

// hasHealthy reports whether a link type is present and usable.
func (b *scoreBoard) hasHealthy(t model.LinkType) bool {
	if s, ok := b.findType(t); ok && s.score >= 0 {
		return true
	}
	return false
}

func (b *scoreBoard) healthyCount() int {
	c := 0
	for i := 0; i < b.n; i++ {
		if b.sc[i].score >= 0 {
			c++
		}
	}
	return c
}

// secondBest finds the best link excluding exclude.
func secondBest(b *scoreBoard, exclude model.LinkType, wantEligible bool) linkScore {
	second := linkScore{score: -2}
	for i := 0; i < b.n; i++ {
		s := b.sc[i]
		if s.score < 0 || s.eligible != wantEligible || s.link == exclude {
			continue
		}
		if s.score > second.score {
			second = s
		}
	}
	return second
}

// reevaluate applies the decision to an existing flow with hysteresis and
// returns whether its primary link changed.
func (e *Engine) reevaluate(f *model.FlowState, d Decision) bool {
	cur := f.Primary
	// 复制状态直接跟随决策（复制的启停不需要滞回，本身就是应急）
	f.Replica = d.Replica

	if d.Primary == "" || d.Primary == cur {
		f.BadSince = time.Time{}
		return false
	}
	curScore, _ := e.scratch.findType(cur)
	candScore, _ := e.scratch.findType(d.Primary)

	// 现链路仍满足 SLA 且候选优势不超过滞回门限 -> 不迁，避免来回切
	if curScore.eligible && candScore.score-curScore.score < e.policy.MigrateScoreMargin {
		f.BadSince = time.Time{}
		return false
	}
	// 现链路不满足 SLA：必须持续劣化满 HoldDown 才迁
	if !curScore.eligible {
		if f.BadSince.IsZero() {
			f.BadSince = e.now
			return false
		}
		if e.now.Sub(f.BadSince) < e.policy.HoldDown {
			return false
		}
	}
	// 执行迁移
	e.migrate(f, d.Primary, d.Reason)
	return true
}

func (e *Engine) migrate(f *model.FlowState, to model.LinkType, reason string) {
	from := f.Primary
	f.Primary = to
	f.Since = e.now
	f.BadSince = time.Time{}
	f.Migrating = true
	// 非实时流：迁移后开启一个重排窗口，接收端按序号缓冲
	prof := e.policy.Profiles[f.App]
	if prof == nil || !prof.Realtime {
		f.ReorderSeq++
	}
	e.pushEvent(model.FlowEvent{
		Flow: f.Key, App: f.App, From: from, To: to,
		Replica: f.Replica, Reason: "migrate:" + reason,
	})
}
