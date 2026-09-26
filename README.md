# Execution Cost Estimator (Go)

本地执行成本估算服务：接收订单簿深度快照/增量与订单执行计划，输出每个时间切片的
**滑点（slippage）**、**市场冲击成本（market impact）**与**残留风险（residual risk）**，
并支持执行策略约束、计划更新/取消、并发一致性与重启恢复。

无外部依赖，仅使用 Go 标准库。

```
go test ./... -race
```

## 快速开始

```go
cfg := estimator.Config{
    Symbol: "BTC-USD", ImpactCoeff: 0.1, ImpactExponent: 0.5,
    DecayRate: 0.3, MaxSlices: 12, MinSliceSeconds: 60,
    Volatility: 0.02, RiskAversion: 1.5, Seed: 42,
}
svc, _ := estimator.NewService(cfg, estimator.NewFileStore("data/state.json"))
ctx := context.Background()

// 1) 先喂全量快照，再喂增量（seq 必须单调）
_ = svc.ApplyDepth(ctx, estimator.DepthUpdate{
    Symbol: "BTC-USD", Seq: 1, Snapshot: true,
    Bids: []estimator.PriceLevel{{99, 5}, {98, 8}},
    Asks: []estimator.PriceLevel{{101, 5}, {102, 8}},
})

// 2) 一次性估算（不落库）或创建可追踪计划
report, err := svc.Estimate(ctx, estimator.EstimateRequest{
    Symbol: "BTC-USD", Side: estimator.Buy,
    Quantity: 4, Horizon: 10 * time.Minute,
    Strategy: estimator.TWAP, Seed: 42, // 可选；不传则用 Config.Seed
})

plan, _ := svc.CreatePlan(ctx, req)
_ = svc.Settle(ctx, plan.ID, 0, 1.25)   // 结算第 0 片（实际成本）
_ = svc.UpdatePlan(ctx, plan.ID, 8, 20*time.Minute) // 调整剩余计划
_ = svc.CancelPlan(ctx, plan.ID)        // 取消后续切片
```

## 模型假设与公式

中价 `mid = (bestBid + bestAsk)/2`。买入吃卖盘、卖出吃买盘，按价格优先逐档成交，
每片的 `FillPrice` 是该切片数量在簿上的成交量加权均价（VWAP）。

对切片 *i*（数量 `q_i`，参与率 `p_i = q_i / ADV_proxy`）：

- **滑点** `S_i = q_i · |vwap_i − mid|`：纯订单簿走簿成本（点差 + 吃深度），确定性。
- **临时冲击** `I_temp_i = η · p_i^γ · (1 + ε_i)`，`ε_i ~ N(0, (0.1σ)²)`，由种子驱动。
- **持久冲击** `I_persist_i = Σ_{j<i} η · p_j^γ · (1−δ)^(i−j)`，按切片指数衰减。
- **冲击成本** `I_i = (I_temp_i + I_persist_i) · mid · q_i`。
- **残留风险** `R_i = remain_i · mid · σ · √(t_frac) · λ`，`t_frac` 为该切片后剩余时间比例。
- 单切片总成本 `S_i + I_i`；买卖两侧都以非负成本幅度上报。

`ADV_proxy` 取当前快照同侧可见深度总量（无外部成交量数据时的静态参与率代理）。
所有成本单位为报价货币（价格 × 数量）。

## 参数（`estimator.Config`）

| 参数 | 含义 | 默认值 |
| --- | --- | --- |
| `ImpactCoeff` (η) | 冲击系数，线性缩放临时/持久冲击 | 0.1 |
| `ImpactExponent` (γ) | 参与率指数（0.5 即平方根冲击定律） | 0.5 |
| `DecayRate` (δ) | 持久冲击的每切片衰减率，范围 [0,1] | 0（需显式设置） |
| `MaxSlices` | 单个计划的最大切片数上限 | 1000 |
| `MinSliceSeconds` | 每片代表的最小秒数，决定切片数 | 60 |
| `Volatility` (σ) | 全程波动率（分数），用于风险与噪声 | 0.01 |
| `RiskAversion` (λ) | 残留风险厌恶系数 | 1 |
| `Seed` | 默认随机种子；请求级 `Seed` 可覆盖 | 1 |

切片数 `n = min(MaxSlices, ceil(Horizon / MinSliceSeconds))`。

### 执行策略（切片权重，归一化后严格和为 1）

- `TWAP`：等权切片。
- `VWAP`：确定性 U 型日内分布（开盘/收盘重、午间轻），`w(x)=1−2.4x(1−x)`。
- `IS`：实施短差，指数前置 `w_i ∝ e^{−i}`。

## 深度序列号语义（乱序 / 重复 / 缺失）

- **快照**（`Snapshot=true`）在任意 seq 到达都会把簿**精确重置**为该图像并清空缓冲，
  是唯一的锚点。
- **增量**只能以 `lastSeq+1, +2, …` 的连续区间应用：提前到达的增量按 seq 缓冲，
  缺失的前序到达后整段按序补齐。
- `seq ≤ lastSeq` 的增量视为**重复/迟到**，直接忽略。
- 永久缺失的增量使簿停留在上一个完整（较旧）图像，直到下一帧快照重新锚定；
  缓冲中早于快照的旧增量绝不会在重置后回放。
- 因此无论到达顺序如何，簿状态都等于“丢弃重复/迟到后对完整序列的全量重建”结果。

## 明确的降级与拒绝

缺失深度**绝不**被当作零深度：

- 从未收到快照、快照为空或只有单边 → `ErrDepthUnavailable`（服务明确不可用）。
- 可见深度不足以吸收某切片数量 → `ErrDepthInsufficient`（`walk` 返回 false）。
- 参数非法（数量/时间为非正、方向或策略未知、symbol 不符）→ `ErrInvalidArgument`。

错误均为可 `errors.Is` 判别的哨兵错误，调用方不会收到静默的误导性零成本。

## 并发一致性

`Service` 用一把读写锁保护深度：估算/建计划全程持读锁并基于一份**完整拷贝**的
`DepthSnapshot` 计算，深度更新持写锁；估算永远不会读到字段级混合的中间状态。
计划生命周期由独立互斥锁保护，加锁顺序恒为 `rwMu → plansMu`，落盘在释放锁之后进行
（先在锁内捕获一致状态，再原子写盘）。

## 计划更新、取消与可追溯

- `Settle` 使切片进入不可变 `SETTLED` 状态，`RealizedCost` 永久保留；重复结算返回
  `ErrSliceSettled`。全部切片结算后计划自动 `COMPLETED`。
- `UpdatePlan` 保留已结算切片原样（含成本与时间偏移），仅对剩余数量/时间用当前快照
  重新投影；不得把总量缩到低于已结算量。
- `Cancel` 把所有未结算切片置为 `CANCELED` 并清零其风险，后续切片**不计入**
  `TotalCost` / `RemainingRisk`；`SettledCost` 仍可追溯。

## 重启恢复

`FileStore` 以 `临时文件 + fsync + rename` 原子提交整份状态（深度 + 全部计划）。
重启后：

- 深度恢复到最近提交的 seq 与图像，故 `seq ≤ lastSeq` 的迟到/重复增量仍被忽略，
  估算不会漂移；若重启前存在未闭合缺口，簿保持上一个完整图像直到下一帧快照。
- 每个计划精确恢复 `SETTLED/PENDING/CANCELED` 切片、成本、策略、种子与基准深度 seq，
  未完成计划的进展（已结算 vs 待执行）清晰可辨。

## 可复现性

临时冲击噪声来自 `rand.New(rand.NewSource(seed))` 的**固定消费顺序**，相同
（请求、快照、种子）逐条一致；`EstimateRequest.Seed` 为 0 时回退到 `Config.Seed`。
走簿滑点、权重、参与率、持久冲击与残留风险均为确定性浮点计算，不使用任何随机源。
跨进程重启后，已投影切片按字节级恢复，逐条保持一致。

## 包结构

| 文件 | 职责 |
| --- | --- |
| `types.go` | 领域类型、配置、请求/报告结构与哨兵错误 |
| `depth.go` | 序列号重建的订单簿、快照拷贝、走簿 |
| `impact.go` | 策略权重、确定性 RNG、滑点/冲击/风险投影 |
| `plan.go` | 计划生命周期（结算/更新/取消）与聚合报告 |
| `service.go` | 并发控制、估算/计划 API、状态捕获 |
| `store.go` / `filestore.go` | 持久化接口与原子 JSON 文件实现 |
| `*_test.go` | 乱序、深度不足、并发、取消、更新、重启、可复现性测试 |
