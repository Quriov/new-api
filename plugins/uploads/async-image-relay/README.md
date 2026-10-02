# async-image-relay —— 把任务式出图请求改发到上游的「异步出图」接口

**这是一个要从后台上传的任务插件，不随镜像内置**（内置的只有 `plugins/tasks/` 下的；本目录不参与 `go:embed`）。
合并这个目录不会改变任何实例的行为；只有管理员在后台上传并启用它、再建一条绑定它的渠道，才会生效。

## 它做什么

客户照旧走任务式接口：`POST /v1/videos` 提交 → `GET /v1/videos/{id}` 轮询 → 拿图片地址。
有的上游不提供 `/v1/videos`，它的出图接口是另一对路径。插件在中间做转换：

| | 客户发给我们的 | 插件发给上游的 |
|---|---|---|
| 提交 | `POST /v1/videos` | `POST {渠道 base_url}/v1/images/generations/async` |
| 查询 | `GET /v1/videos/{id}`（读库里的最新快照） | 后台轮询 `GET {渠道 base_url}/v1/images/generations/async/{上游任务号}` |

上游地址和密钥只来自渠道配置，插件里不写任何地址。

## 参数怎么映射

| 客户字段 | 发给上游 | 说明 |
|---|---|---|
| `model`（对外型号名） | `model` | 默认见下表；渠道配了「模型映射」就以渠道为准。**档位永远按客户填的对外名定** |
| `aspect_ratio`（也认 `metadata.aspect_ratio`） | `size`（像素 `宽x高`） | 上游按像素收，不认比例。不传 / `auto` 按 `1:1` |
| `size` | `size` | 只收该型号这一档尺寸表里的像素，别的直接拒（防止拿低档型号要高档像素） |
| `image_size` / `resolution` | 不发 | 只用来核对：跟型号的档位不一致就拒 |
| `images` / `image` / `input_reference` / `metadata.urls` | `image`（数组） | 合并去重，最多 8 张。**只收公网 `http(s)` 地址**；base64、`data:`、文件上传一律拒 |
| `quality` | `quality` | 原样带过去；取值不在该型号的范围里就拒。不传则不发 |
| `n` | `n: 1` | 按次计价，一次一张；传别的数直接拒 |
| — | `response_format: "url"`、`watermark: false` | 固定带上：结果要地址不要 base64；不要水印 |

其余客户字段不转发。

| 对外型号 | 档位 | 默认上游型号 | 可用比例 |
|---|---|---|---|
| `gpt-image-2` | 1K | `gpt-image-2` | 1:1、4:3、3:2、2:3、16:9、9:16 |
| `gpt-image-2-2K` / `-4K` | 2K / 4K | `gpt-image-2-pro` | 同上 |
| `gpt-image-2.5-1K` / `-2K` / `-4K` | 1K / 2K / 4K | `gpt-image-2.5-flare` | 上面六个，加 3:4、4:5、5:4、21:9 |

各档每个比例对应的像素见 `plugin.js` 顶部的 `BASE_SIZES` / `EXTRA_SIZES`。

## 状态怎么映射

| 上游查询结果 | 任务状态 |
|---|---|
| `status: queued` | 排队中 |
| `status: processing` / `in_progress` | 进行中（带上游给的百分比） |
| 成品对象（有 `data[]`，通常没有 `status`）或 `completed` / `succeeded` | 成功 —— 前提是 `data[].url` 有地址、且尺寸够档位（见下） |
| `status: failed` / `cancelled` | 失败 |
| 其它认不出的响应 | 不当成「还在跑」：交给宿主按连续轮询失败计数，到上限判失败 |

成功后客户查询拿到 `url`、`image_url`（同值）、`data[].url`、`size`；`id` / `status` / `model`（对外名）等由宿主填。上游的 token 用量不外露。

## 失败怎么报

失败不扣费、提交即拒不产生任务，这些都是宿主既有的行为，插件只负责把失败如实报出来，不吞错：

| 情况 | 结果 | 客户看到的原因 |
|---|---|---|
| 比例 / 尺寸 / 档位 / 张数 / 参考图格式不支持，缺 `prompt` | 发上游之前就拒（HTTP 400），不扣费 | 具体哪一项不支持，以及支持的取值 |
| 上游拒绝提交（非 2xx，或 2xx 但正文是错误 / 没有任务号） | 提交失败，不产生任务，不扣费 | 上游给的错误信息 |
| 上游任务失败 / 被取消 | 任务失败，退款 | `upstream task failed: <上游原因>` |
| 提交后 600 秒还没出结果 | 任务失败，退款 | `upstream did not finish within 600 seconds (...)` |
| 完成了但没有图片地址 | 任务失败，退款 | `upstream finished the task without an image URL` |
| 上游回报的尺寸不够档位（长边：1K ≥ 1000、2K ≥ 2000、4K ≥ 2800） | 任务失败，退款，**不把那张图的地址交给客户** | `upstream returned a 1254x1254 image, below the 2K tier ...` |
| 2K / 4K 完成了但上游没回报尺寸 | 任务失败，退款（没法确认就不当成功） | `upstream result did not report the image size ...` |

任务失败后宿主的「换渠道重投」照常工作：同型号还有别的渠道时会换过去再试一次，钱只收一次。

⚠ **「尺寸够不够」看的是上游在结果里回报的 `size`，不是量出来的像素** —— 任务插件不能下载文件。上游要是回报的尺寸跟实际出图不一致，这道检查拦不住。

## 启用前要知道的

1. **顺序只能是「先启用插件、后建渠道」**：渠道绑定时要求插件已经在运行。两步之间这些型号没有可用渠道（提交回 503，不扣费），所以两步要挨着做。
2. **启用期间，这六个型号的首次提交只会发给绑定了本插件的渠道**。原来的渠道即使优先级更高也不再接它们的首次提交，只在任务失败后「换渠道重投」时才用得上。
   想只切一部分型号，就在上传前把 `plugin.js` 里 `MODELS` 表删到只剩要切的那几行。
3. 这些型号要按次固定价收费：照旧列在 `TASK_PRICE_PATCH` 里（插件不报用量）。
4. 回退 = 在后台停用插件（连带停用绑定它的渠道），路由立刻回到原来的渠道；有在飞任务时后台会拦一下，等它们结束再停。

## 测试

```bash
# 单测：Go 加载这份 JS，对本地假上游跑完整的 提交 → 轮询 → 取图 链路
go test ./relay/channel/task/jsplugin/ -run AsyncImagePlugin

# 启用 / 回退演练：真起一个实例（SQLite），后台上传插件、建渠道，客户走 /v1/videos；只连本机两个 mock
python3 scripts/quriov-e2e/mock_upstream.py 18090 &
python3 scripts/quriov-e2e/mock_async_image_upstream.py 18091 &
go build -o /tmp/new-api . && python3 scripts/quriov-e2e/async_image_plugin_e2e.py --bin /tmp/new-api \
    --legacy-mock-port 18090 --async-mock-port 18091
```

改动映射或状态解析后，上线前先用真上游各档打一张、下载图片量像素 —— 单测只能证明「实现的是我以为的协议」。
