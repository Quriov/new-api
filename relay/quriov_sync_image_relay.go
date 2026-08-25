package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// 同步出图上游的协议转换（Quriov 改造）。
//
// 要解决的问题：我们的客户走异步 `/v1/videos` 提交图片任务，所以只有同样支持
// 异步任务接口的上游才进得来。这把一整家上游挡在了门外 —— 而那恰恰是唯一一家
// 跟主通道**不同源**的备份线。主通道整家塌掉时（2026-08-21 实际发生过），
// 我们没有腿可换，客户 16 个任务全灭、停手三小时。
//
// 为什么现在可以做了：这条转换只用在【任务失败后换渠道重投】那条路上，而那条路
// 跑在后台轮询协程里 —— 前面**没有客户的 HTTP 连接，也没有网关超时**。
// 同步等几十秒完全可以。（当初把出图切成异步，正是为了消掉网关那 100 秒上限；
// 那个约束在这条路上不存在。）
//
// ⚠⚠ 协议是 2026-08-24 对真上游实测出来的，不是照文档写的。第一版照主站
//     image2_relay 的写法走 `/v1/chat/completions`，真打一次直接被拒：
//     "This model is not supported on the Chat Completions endpoint"。
//     **单测当时是全绿的** —— 因为单测测的是我以为的协议。
//     实测结果：走 `/v1/images/generations`，HTTP 200，21–26 秒，
//     `data[0].url` 直接给图片地址；`size` 是一等参数（不用塞进提示词）。

// SyncImageResult 是一次同步出图的结果。
type SyncImageResult struct {
	// ImageURL 上游返回的图片地址。
	ImageURL string
	// RawBody 上游原始响应，落进 task.Data 供排查。
	RawBody []byte
}

// SyncImageRelayTimeout 单次同步出图的上限。
// 实测 21–26 秒；给到 4 分钟留足余量（上游忙时会慢）。
// 设得太短的失败方式是静默的：看起来像"上游慢"，实际是我们自己掐的。
const SyncImageRelayTimeout = 4 * time.Minute

// SyncImageRelayPath 实测通的端点。
const SyncImageRelayPath = "/v1/images/generations"

// BuildSyncImageRequestBody 把异步任务的请求体转换成同步出图的请求体。
//
// 输入是客户提交给 /v1/videos 的原始 JSON，形如：
//
//	{"model":"gpt-image-2","prompt":"...","size":"720x1280"}
//
// 输出是 OpenAI 标准出图格式：{model, prompt, n, size}。
//
// ⚠ 带参考图的请求会被**拒绝**，不会降级成纯文生图 —— 见 ErrSyncRelayNeedsReferenceImages。
func BuildSyncImageRequestBody(originalBody []byte, upstreamModel string) ([]byte, error) {
	var req map[string]any
	if err := common.Unmarshal(originalBody, &req); err != nil {
		return nil, fmt.Errorf("原始请求体不是合法 JSON: %w", err)
	}

	if urls := referenceImageURLs(req); len(urls) > 0 {
		// 这条同步端点是纯文生图。悄悄把参考图丢掉会出一张【构图完全不同】的图，
		// 而它会被当成成功交给客户 —— 那比失败糟得多，因为没人会发现。
		// 宁可这条腿对这类请求不可用。
		return nil, fmt.Errorf("%w（%d 张）", ErrSyncRelayNeedsReferenceImages, len(urls))
	}

	prompt := firstNonEmptyString(req, "prompt", "input", "text")
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("原始请求里没有提示词，无法转换")
	}

	body := map[string]any{
		"model":  upstreamModel,
		"prompt": prompt,
		"n":      1,
	}
	// size 是这个端点的一等参数 —— 实测 720x1280 被正确接受并原样回显。
	// （第一版把它拼进提示词，那是 chat 格式时代的将就做法，现在不需要了。）
	if size := strings.TrimSpace(asString(req["size"])); size != "" {
		body["size"] = size
	}
	return common.Marshal(body)
}

// ErrSyncRelayNeedsReferenceImages 表示这条同步腿接不了带参考图的请求。
// 单独一个 error 值，是为了让调用方能把它跟"上游挂了"区分开 ——
// 前者是能力边界（下次同样的请求还是不行），后者是瞬时故障。
var ErrSyncRelayNeedsReferenceImages = fmt.Errorf("同步腿只支持纯文生图，这个请求带了参考图")

// referenceImageURLs 从原始请求里把参考图地址抠出来。
// 兼容几种常见字段名 —— 客户端和上游对这个字段的叫法一直不统一。
func referenceImageURLs(req map[string]any) []string {
	var out []string
	for _, key := range []string{"images", "image_urls", "image", "reference_images"} {
		switch v := req[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				out = append(out, s)
			}
		case []any:
			for _, item := range v {
				if s := strings.TrimSpace(asString(item)); s != "" {
					out = append(out, s)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}

// ParseSyncImageResponse 从同步上游的响应里取出图片地址。
//
// 实测响应形状（2026-08-24，真上游）：
//
//	{"created":…,"data":[{"revised_prompt":"…","url":"https://…/x.png"}],"size":"720x1280","usage":{…}}
func ParseSyncImageResponse(body []byte) (*SyncImageResult, error) {
	var resp struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := common.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON: %s", truncate(string(body), 200))
	}
	if resp.Error != nil && strings.TrimSpace(resp.Error.Message) != "" {
		return nil, fmt.Errorf("上游报错: %s", truncate(resp.Error.Message, 300))
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("上游响应里没有 data: %s", truncate(string(body), 200))
	}
	if url := strings.TrimSpace(resp.Data[0].URL); url != "" {
		return &SyncImageResult{ImageURL: url, RawBody: body}, nil
	}
	if resp.Data[0].B64JSON != "" {
		// 上游给的是内嵌 base64 而不是地址。我们这条路没有落盘的地方,
		// 硬塞进 task.Data 会让那一行变成几 MB。明确拒绝, 别假装成功。
		return nil, fmt.Errorf("上游返回的是内嵌 base64 而不是图片地址，这条路不支持")
	}
	return nil, fmt.Errorf("上游返回的 data 里既没有 url 也没有 b64_json: %s", truncate(string(body), 200))
}

// CallSyncImageRelay 同步调用一个只有出图接口（没有异步任务接口）的上游。
//
// ⚠ 这个调用会阻塞几十秒。只能在后台协程里用，绝不能放进客户请求链路
// （那里有网关超时，正是当初切异步要躲的东西）。
func CallSyncImageRelay(ctx context.Context, ch *model.Channel, originalBody []byte, upstreamModel string) (*SyncImageResult, error) {
	if ch == nil {
		return nil, fmt.Errorf("渠道为空")
	}
	reqBody, err := BuildSyncImageRequestBody(originalBody, upstreamModel)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(ch.GetBaseURL(), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("渠道 #%d 没有配置 base_url", ch.Id)
	}

	callCtx, cancel := context.WithTimeout(ctx, SyncImageRelayTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, baseURL+SyncImageRelayPath, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	key, _, apiErr := ch.GetNextEnabledKey()
	if apiErr != nil {
		return nil, fmt.Errorf("取渠道 #%d 的 key 失败: %s", ch.Id, apiErr.Error())
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: SyncImageRelayTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求同步上游失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读同步上游响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("同步上游返回 HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	return ParseSyncImageResponse(respBody)
}

// ── 小工具 ────────────────────────────────────────────────────────────

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmptyString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := strings.TrimSpace(asString(m[k])); s != "" {
			return s
		}
	}
	return ""
}
