# QURIOV.md — 这个 fork 相对上游改了什么

> 本仓是 [QuantumNous/new-api](https://github.com/QuantumNous/new-api) 的**冻结改造分支**。
> 冻结点：`2d8e50bf36e9`（上游 2026-08-21 15:47 UTC 的最新提交）。
>
> - `main` = **纯上游镜像**，不放任何自己的东西，保证随时能干净 fast-forward 同步。
> - `quriov` = **我们的改造线**，默认分支。上游更新了就去看看有没有值得摘的，而不是自动跟。
>
> **这个文件是给下一个人看的**：改动散在源码各处，光看 diff 很难拼出「为什么」。
> 每加一处改造，回来加一节，写清楚**它治的是哪个真实问题**。

## 许可证（动手前先看这条）

上游是 **AGPL-3.0 + 第 7 条附加条款**。对我们意味着三件硬约束：

1. 改过的版本对外提供网络服务 → 必须向使用该服务的用户提供改过的源码。
   本仓是**公开仓**，`quriov` 分支保持公开即满足，成本几乎为零。
2. **必须保留** New API 的署名（`Frontend design and development by New API contributors.`），
   位置在页脚 / 关于 / 法律声明这类显著处。**「删掉署名换成自己的」不是选项。**
3. **必须保留**指向上游 <https://github.com/QuantumNous/new-api> 的可见链接。

⇒ **放进这个 fork 的任何东西都会变成公开的。** 所以只放**通用机制**；
价格数字、利润系数、渠道优先级策略、上游厂商真名映射这类东西一律留在我们自己的服务和配置里。
一句话判据：**泄露了会让我们吃亏的，不进这个仓。**

---

## 改造清单

### 1. 任务失败后换渠道重投（2026-08-22）

**治的问题**：上游把异步任务**收下之后**才失败的那一类，原本完全没有兜底。
new-api 的渠道重试只包住提交阶段（`controller/relay.go` 的 `relayTaskHandler` 重试循环）——
任务一旦落库返回给客户，之后无论上游怎么失败都是终态，不会去试别的渠道。

**实证**：2026-08-21 某天 **28 次任务失败全部落在同一个渠道上**，
旁边配好并且启用着的备用渠道**一次都没有被试过**（那个渠道 7 天总共只跑了 4 个任务）。

**做了什么**：轮询判定任务失败时，如果这个失败看起来是「换一家上游有可能成功」的那一类，
就挑一个还没试过的渠道，把**留存的原始请求**重发一次，让任务继续走轮询。

**刻意不做的**：

- **不新建任务行** —— 客户手上的 `task_id` 不变，他不需要知道我们在底下换了腿。
- **不重新计费** —— 额度在首次提交时已预扣，重投沿用同一笔：最终成功才结算、最终失败才退款。
  **换腿不会让客户被收两次钱。**
- **内容审核类失败不重投** —— 换家上游一样会拒，重投只是把一次收费变两次、还多等一轮。
- **超时清扫路径（`sweepTimedOutTasks`）没接** —— 默认 24 小时才判超时，那时候重投对客户已无意义。要接再议。

**碰了哪些文件**：

| 文件 | 干什么 |
|---|---|
| `constant/env.go` · `common/init.go` | 4 个配置项（见下表） |
| `model/task.go` · `model/task_resubmit.go` | 任务上留存原始请求 + 已试渠道清单 |
| `controller/relay.go` | 提交成功落库时留存原始请求 |
| `controller/task_resubmit.go` | 用留存的请求造一个等价上下文，走**真实那条提交链路** |
| `relay/relay_task_resubmit.go` | 打新渠道的那一段（`RelayTaskSubmit` 去掉计费） |
| `service/task_resubmit.go` | 判据 + 挑渠道 + 编排 |
| `service/task_polling.go` | 失败分支上的钩子 |
| `main.go` | 接线（跟 `GetTaskAdaptorFunc` 同一个套路，不破坏 service→relay 依赖方向） |

**配置**（都走环境变量）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `TASK_RESUBMIT_ENABLED` | `true` | 总开关。**默认开**是刻意的——默认关会造出「代码写好了但没在起作用」这种最难发现的空转 |
| `TASK_RESUBMIT_MAX_ATTEMPTS` | `1` | 单个任务最多额外重投几次 |
| `TASK_RESUBMIT_MAX_BODY_KB` | `256` | 超过这个大小的请求体不留存，该任务也就失去重投能力（宁可如此，也不要把几 MB 图片塞进 tasks 表） |
| `TASK_RESUBMIT_SKIP_REASONS` | 见 `common/init.go` | 逗号分隔。失败原因命中即判定「换家上游也没用」，直接终态。大小写不敏感 |

**怎么验它真的在起作用**（不是「部署了」）：

```bash
# 换过腿的任务：tried_channels 里会有两个 ID，resubmit_count > 0
SELECT task_id, channel_id, status,
       JSON_EXTRACT(private_data,'$.tried_channels')  AS 试过的渠道,
       JSON_EXTRACT(private_data,'$.resubmit_count')  AS 重投次数
FROM tasks
WHERE JSON_EXTRACT(private_data,'$.resubmit_count') > 0
ORDER BY created_at DESC LIMIT 20;
```

日志里搜「已从渠道」也能看到每一次换腿。

**测试**：`service/task_resubmit_test.go`（15 条，判据层）+
`service/task_resubmit_polling_test.go`（2 条，**从轮询这一层进来**，盯的是钩子有没有真的接上——
只有前者的话，把 `task_polling.go` 里那行调用删掉，测试依然全绿）。

### 2. 让「只有同步接口」的上游也能当备份腿（2026-08-24）

**治的问题**：我们的客户走异步 `/v1/videos` 提交图片任务，所以**只有同样支持异步的上游才进得来**。
这把一整家上游挡在了门外 —— 而那恰恰是唯一一家跟主通道**不同源**的备份线。

实证：api 站的两个渠道虽然是两个域名，但出图落在**同一个 OSS 桶**，是同一家上游的两个门面。
主通道整家塌掉时（2026-08-21 实际发生过），改造 1 的「换渠道重投」**换过去还是死**——
路修好了，但没有车。

**关键洞察（也是这条改动能这么小的原因）**：

当初把出图从同步切成异步，是为了消掉网关那 100 秒超时（同步出图要 44–90 秒）。
但**重投跑在后台轮询协程里 —— 前面没有客户的 HTTP 连接，也没有网关**。

```
客户 →(HTTP，网关 100 秒上限)→ api 站 → 立刻返回任务 ID
                                  ↓
                        后台轮询协程每隔一会儿查一次
                                  ↓
                        失败 → 换渠道重投   ← 没有网关，可以同步等
```

⇒ **不需要造通用的「同步包装成异步」适配器**（那是个大工程）。只在重投这条路上直接同步调。

**做了什么**：

- 渠道设置加一个开关 `quriov_sync_image_relay`。标了它的渠道：
  - **对正常路由完全不可见** —— 客户首次请求发给它必然失败，让它进正常路由 = 主动制造故障
  - 只在「任务失败后换渠道重投」时被单独挑出来，且**排在所有异步渠道之后**
    （先试便宜的，实在没得换才上要阻塞几十秒的它）
- 协议转换：异步任务的请求体 → `POST /v1/images/generations` 的 `{model, prompt, n, size}`
  - **`size` 是一等参数**，不用拼进提示词
  - **带参考图的请求直接拒绝**，不降级成纯文生图 —— 悄悄丢掉参考图会出一张
    构图完全不同的图，而它会被当成成功交给客户，那比失败糟得多
- 响应解析：取 `data[0].url`；空 url、只有 base64、上游报错 **全部判失败**，不假装成功

> 🔴 **协议是实测出来的，不是照文档抄的。** 第一版照主站 `image2_relay` 的写法走
> `/v1/chat/completions`，**单测全绿**，对真上游打一次直接被拒：
> `"This model is not supported on the Chat Completions endpoint"`。
> 实测结果：`/v1/images/generations`，HTTP 200，**21–26 秒**，`data[0].url` 给地址，
> `size` 传 `720x1280` 原样回显。
> ⇒ **改这段协议之前请先对真上游打一次** —— 单测只能证明「我实现的是我以为的协议」。
- 重投结果从布尔改成三态：`None` / `Async`（换了腿继续轮询）/ `Completed`（同步腿已出图）。
  `Completed` 走**结算**而不是退款 —— 客户拿到图了，这笔钱该收

**为什么阻塞几十秒是可以接受的**：轮询按渠道并行（每渠道一个 goroutine），
所以只拖同渠道排在后面的任务；且系统任务的租约靠后台心跳续，**这一轮没有固定超时**。

**碰了哪些文件**：`relaykit/dto/channel_settings.go`（开关）·
`model/channel_cache.go`（正常路由隐藏 + 专用查询）· `relay/quriov_sync_image_relay.go`（协议转换）·
`service/task_resubmit.go`（三态 + 同步腿分支）· `service/task_polling.go`（结算而非退款）·
`controller/task_resubmit.go` + `main.go`（接线）

**怎么启用**：给那个上游建一个普通渠道，模型填 `gpt-image-2`，渠道设置里加
`{"quriov_sync_image_relay": true}`。**不加这个开关它会进正常路由，客户首次请求就会失败。**

**怎么验它真的在用**：

```sql
-- 换到同步腿并成功出图的任务
SELECT task_id, channel_id, status,
       JSON_EXTRACT(private_data,'$.tried_channels') AS 试过的渠道,
       JSON_EXTRACT(private_data,'$.resubmit_count') AS 重投次数
FROM tasks WHERE JSON_EXTRACT(private_data,'$.resubmit_count') > 0 AND status='SUCCESS'
ORDER BY created_at DESC LIMIT 20;
```

日志里搜「换到同步腿」。

**测试**：`relay/quriov_sync_image_relay_test.go`（13 条，协议转换与响应解析，**用真上游实测响应当样本**）+
`service/task_resubmit_test.go` 里的同步腿一组（5 条）。
其中最硬的一条是 `TestSyncRelayChannel_IsInvisibleToNormalRouting` ——
把同步腿的优先级**故意设成最高**，断言正常路由仍然选不到它。
