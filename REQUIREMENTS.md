# 需求覆盖矩阵

每条原始需求对应到代码实现与自动化测试，便于评审验收。

## 仿真网络真实性

| 需求 | 实现 | 测试 |
|---|---|---|
| 每条链路时延/丢包/抖动/带宽随时间变化 | `network/link.go` OU 均值回归噪声 + 排队模型 | `probe/tracker_test.go`、`sim/sim_test.go` |
| 宽带晚上质量变差 | `link.go` `eveningFactor`/`dialFactor`，19:00–24:00 RTT↑ 容量↓ | `TestEveningBroadbandWorse` |
| 移动网按流量计费 | `model.Policy.CostPerGB`，遥测与报告累计费用 | 验收报告第 3 节 |
| 几百门店 + 十几区域仓库，专线/宽带/部分移动网卡 | `network/topology.go`，10 区域、300 店、约 40% 含移动网卡 | `TestFluidRunsAndProducesMetrics` |
| 注入专线拥塞/宽带故障等 | `sim/faults.go` 确定性时间表 + 容量压缩/中断/恢复 | `TestFaultInjectionTogglesOutage`、`TestFaultRestoresAfterDuration` |

## 1. 应用流量识别

| 需求 | 实现 | 测试 |
|---|---|---|
| 目的地址 + 端口规则 | `classify` 阶段①，CIDR 前缀 + 端口集 | `TestPortRuleClassification` |
| 首包特征 | 阶段③，ISO8583/RTP/TLS SNI/rsync 签名 | `TestSignatureClassification` |
| 域名解析关联 | 阶段②，DNS 响应绑定 IP→应用 | `TestDNSAssociation` |
| 识别不了按行为归类 | 阶段④，包长/间隔/累计字节启发式 | `TestBehavioralFallback`、`TestBehaviorPeriodicSmall` |
| 识别结果缓存到流结束 | 流级 verdict 缓存，`End` 即释放 | `TestVerdictCachedUntilFlowEnds` |
| 识别准确率（对照真值） | 生成器带真值，验收统计 | 300 店报告 100% |

## 2. 持续链路探测

| 需求 | 实现 | 测试 |
|---|---|---|
| 边缘间每条链路持续发探测包 | `probe.Tracker`，每链路每秒一探；包级模式真实入队 | `TestTrackerMeasuresRTTAndLoss` |
| 实测实时 RTT/丢包/抖动 | EWMA + 30 探测滑动丢包窗口 | `TestProbeAckRTT` |
| 故障检测 | 连续全丢/中断→unhealthy，恢复自动回切 | `TestTrackerDetectsOutage` |

## 3. 按质量选路 + 滞回迁移

| 需求 | 实现 | 测试 |
|---|---|---|
| 每类应用质量要求与链路偏好 | `model.AppProfile` SLA 门限 + 偏好序 | 全 profile 单测 |
| 为新流按实时质量选链路 | `edge.chooseLink` 0–100 综合评分 | `TestBaselineAlwaysMPLS` 等 |
| 质量变差迁移敏感流 | `reevaluate` 周期重评估 | `TestHysteresisMigrationOnOutage`、`TestPacketEngineMigrationOnOutage` |
| 迁移考虑乱序 | 非实时流开 `ReorderWindow`，实时流直切 | `TestReorderWindowExpires` |
| 滞回避免来回切 | 分数门限 `MigrateScoreMargin` + `HoldDown=20s` | `TestFlapCountsOnlyPingPong`（乒乓占比量化） |

## 4. 双链路复制 / 限速 / 成本

| 需求 | 实现 | 测试 |
|---|---|---|
| 关键应用质量全差时双链路复制 | `chooseLink` 全 SLA 违例/临界质量时启用 Replica | `TestDualReplicationWhenAllLinksBad` |
| 接收端去重 | 流+序号滑动窗口 deduper | `TestReceiverDedup`、`TestDeduperSlidingWindow`、`TestPacketEngineDedupCounters` |
| 大流量限速 | 边缘全局令牌桶 `bulkShaper` | `TestBulkShapedAndCheapLinkOnly`、`TestBulkShaperTokenBucket`、`TestShapedBulkNotSLAViolation` |
| 大流量只走便宜链路 | `cheapType` 过滤，专线仅兜底 | `TestBulkPrefersBroadbandOverCellular`、`TestBulkFallbackToMPLSWhenCheapDown` |
| 非关键应用不复制 | critical 标志门控 | `TestNoReplicationForNonCritical` |

## 5. 中心控制器 + 失联自治

| 需求 | 实现 | 测试 |
|---|---|---|
| 下发策略 | 版本化 `PushPolicy`/`PullPolicy` | `TestPolicyVersioningAndPull`、`TestHTTPRoundTrip` |
| 查看全网状态 | 遥测聚合 `Snapshot`/`/state` | `TestTelemetryAggregation`、`TestStateEndpoint` |
| 失联按最后策略继续工作 | 客户端缓存最后策略，边缘不中断转发 | `TestControllerLossKeepsLastPolicy`、`TestUnreachableServesLastPolicy`、`TestControllerOutageAutonomy` |
| 恢复后重新同步 | 连接恢复自动 pull | `TestControllerReconnectResumes`、`TestHTTPClientFallbackWhenServerDown` |
| 真实设备 HTTP 传输 | `controller/http.go` 服务端 + 客户端 | `TestHTTPRoundTrip` |

## 验收与可视化

| 需求 | 实现 | 测试 |
|---|---|---|
| 全走专线 vs 按质量选路对照 | `cmd/sdwan accept` 同负载双模式 | `TestAcceptanceInvariants` |
| SLA 达标率/专线用量/移动费用/中断时长 | `sim/metrics.go` + `report` Markdown/JSON | 300 店验收报告 |
| 各店各链路实时质量 | `web` 仪表盘链路卡片 | `TestDemoStoresAndSnapshot` |
| 应用流量在链路上分布 | 仪表盘分布表（实测 bps） | web 集成测试 |
| 链路劣化时流迁移 | 事件流 + 故障注入按钮 | `TestDemoFaultInjection`、`TestPacketEngineMigrationOnOutage` |
| 仿真可复现/并行一致 | 种子确定性 + 串行/并行逐字节一致 | `TestDeterminismAcrossWorkerCounts`、`-race` 全绿 |
