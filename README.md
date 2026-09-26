# Execution Cost Estimator (Go)

本地执行成本估算服务：接收订单簿深度快照/增量与订单执行计划，输出每个切片的
**预期滑点、价差成本、市场冲击与剩余风险**，支持 TWAP / VWAP / POV 策略、
计划结算与取消、并发一致性、可复现随机数，以及服务重启后从最近一次已提交
位置恢复。

## 1. 架构

```
cmd/server     HTTP 入口（POST /v1/...）
service        门面：串行提交 + 一致快照读取 + 恢复编排
depth          订单簿：快照重置、乱序缓冲、去重、缺口标记
strategy       切片生成 + 滑点/冲击/剩余风险模型（确定性 RNG）
plan           计划跟踪：结算、取消、深拷贝
persist        原子 JSON 落盘（tmp + rename）
model          共享领域类型与错误
```

## 2. 深度语义（需求 1 / 4）

- `Reset(snapshot)`：以权威快照（含 seq）重置某 symbol。
- `Apply(update)` 严格按 `seq = lastSeq+1` 应用：
  - **重复**（seq ≤ lastSeq）：忽略；
  - **乱序**（seq > lastSeq+1）：进入有界重排缓冲（4096 条），缺口补齐后
    按 seq 连续应用——无论物理到达顺序如何，结果与按序到达完全一致；
  - **缺失**：缺口未闭合期间，订单簿状态始终停留在 `lastSeq` 的完整前缀，
    绝不呈现字段级混合状态；此时 `View` 返回 `ErrStaleDepth`，服务拒绝
    估算（HTTP 503），而不是用落后或编造的深度继续算；
  - 迟到的缺失更新会自愈；新快照能愈合任何缺口并丢弃更旧的缓冲数据。
- `View()` 在读锁内返回**深拷贝**的完整视图。估算先取完整视图再建模，
  因此更新与估算并发时，每次估算只基于某一个一致快照（`-race` 测试覆盖）。

## 3. 成本模型与假设（需求 2 / 6）

线性、透明、可复现。对请求 `(symbol, side, quantity, horizon)`：

- **切片数量**：`min(MaxSlices, ceil(horizon / 1 分钟))`。
- **切片数量分配**：
  - TWAP：均匀；
  - VWAP：对称 U 型日内成交量曲线（开收盘更重），权重归一；
  - POV：每片最多吃 `Participate × ADV`，切片数由参与率驱动，上限
    `MaxSlices`。
- **ADV 代理**：可成交一侧（买单看 asks，卖单看 bids）可见档位总量。
- **价差成本**：`h = (bestAsk − bestBid)/2`，每单位都支付。
- **临时冲击**：`I_i = η × q_i / ADV`（`η = ImpactCoeff`）。前序冲击每
  经过一个时间步衰减：`carry = (carry + I) × (1 − DecayRate)`。
- **预期价格**（方向调整后）：
  - 买：`bestAsk + h + I − carry + noise`
  - 卖：`bestBid − h − I + carry − noise`
- **滑点**：相对到达中间价（arrival mid）的每单位正成本，负值截断为 0。
- **剩余风险**：`剩余数量 × arrival × σ_min × sqrt(剩余分钟)`，其中
  `σ_min = RiskAnnual / sqrt(525600)`（一年分钟数）。
- **噪声**：可选的确定性 xorshift64* 均匀分布，幅度 ±1bp 参考价；
  `RNG=0` 关闭，`RNG=1` 启用。相同输入与 `Seed` 下逐条结果一致
  （`TestReproducibleSameSeed`）。

假设说明：模型假设深度在 horizon 内静态、线性瞬时冲击、冲击按固定比例
指数松弛、无费率与对手方信息；这些是可解释的工程近似，参数全部可配。

### 参数（`Params`，零值用默认值填充）

| 参数 | JSON | 默认 | 含义 |
|---|---|---|---|
| ImpactCoeff | `impact_coeff` | 0.10 | 冲击系数 η |
| DecayRate | `decay_rate` | 0.50 | 每步冲击衰减率 [0,1) |
| MaxSlices | `max_slices` | 20 | 切片数硬上限 |
| RiskAnnual | `risk_annual` | 0.40 | 年化波动率，用于剩余风险 |
| Participate | `participate` | 0.10 | POV 参与率 (0,1] |
| Seed | `seed` | — | 确定性 RNG 种子 |
| RNG | `rng` | 0 (none) | 0 无噪声，1 均匀噪声 |

## 4. 深度不足 / 为空（需求 3）

- **空 / 无快照 / 单边无有效价格**：返回 `ErrEmptyDepth`（HTTP 503），
  绝不把缺失深度当作零深度继续估算。
- **缺口未闭合**：`ErrStaleDepth`（HTTP 503）。
- **深度不足**（可见档位无法覆盖全部数量）：
  - `reject_if_insufficient=true`：返回 `ErrInsufficientDepth`（HTTP 409）；
  - 否则返回 200 且报告带非空 `degraded` 字段（原因、可用量、请求量），
    显式降级，不静默返回误导性结果。

## 5. 计划更新、结算与取消（需求 5）

- 估算成功即创建 `Plan` 并持久化。
- `Settle(plan, i)`：标记第 i 片已结算（幂等）；已取消计划拒绝结算。
  全部非取消片结算完计划自动 completed。
- `Cancel(plan)`：所有 pending 片置为 cancelled；已结算片保留可追溯，
  取消片**不计入** `total_cost / total_impact` 与残留风险（残留风险取
  最后一个非取消片的值）。

## 6. 持久化与重启恢复（需求 7）

- 每次快照、更新、创建计划、结算、取消都通过 `persist.Store` 提交：
  先写 `state.json.tmp` 再 rename，读端永远看到完整提交。
- 重启时 `Recover()` 恢复全部计划（含每片状态），并为每个 symbol
  **预置最后已提交 seq 但不编造档位**：在新快照到达前估算一律 503；
  迟到/重复的旧更新（seq ≤ 已提交位置）被丢弃，已缓冲的未来更新在快照
  到达后自动重放，不产生估算漂移。未完成计划保持 active、已取消保持
  cancelled，进展清晰可辨。

## 7. HTTP API

```
POST /v1/depth/snapshot        {"symbol","seq","bids":[{price,size}],"asks":[...]}
POST /v1/depth/updates        增量，size=0 表示删除该价位
POST /v1/estimates            {"symbol","side":1买|2卖,"quantity","horizon_ms",
                              "strategy":1TWAP|2VWAP|3POV,"params":{...},
                              "reject_if_insufficient":false}
GET  /v1/plans/{id}
POST /v1/plans/{id}/settle    {"slice_index":n}
POST /v1/plans/{id}/cancel
GET  /healthz
```

错误码：400 参数错误；404 计划不存在；409 深度不足/计划已取消；
503 深度为空或存在 seq 缺口。

## 8. 构建与测试

```bash
go build ./...
go test -race ./...
go run ./cmd/server -addr :8080 -state ./data/state.json
```

测试覆盖：深度乱序/重复/缺口自愈/快照重置（`depth`），可复现性与三策略
（`strategy`），结算/取消/防御性拷贝（`plan`），原子持久化（`persist`），
以及服务级的深度不足、并发估算与更新、取消、重启恢复、逐条可复现
（`service`，含 `-race`）。
