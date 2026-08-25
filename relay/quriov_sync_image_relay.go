package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
// ⇒ 因此**不需要**造一个通用的「同步包装成异步」的东西。只在重投这条路上直接同步调。

// SyncImageResult 是一次同步出图的结果。
type SyncImageResult struct {
	// ImageURL 上游返回的图片地址。
	ImageURL string
	// RawBody 上游原始响应，落进 task.Data 供排查。
	RawBody []byte
}

// : 同步出图上游把结果放在 choices[0].message.content 里，形如
// : "![image](https://oss.example.top/uploads/<uuid>.png)\n\n"。
// : 只取第一个 markdown 图片链接 —— 有些提示词会返回多段文字，图片块可能在首行或末行。
var markdownImageURLPattern = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^\s)]+)\)`)

// : 兜底：有些上游不裹 markdown，直接给一个裸链接。
var bareImageURLPattern = regexp.MustCompile(`https?://[^\s"'` + "`" + `<>]+\.(?:png|jpe?g|webp)(?:\?[^\s"']*)?`)

// SyncImageRelayTimeout 单次同步出图的上限。
// 实测这类上游出一张图要 44–90 秒，给到 4 分钟留足余量；
// 超了就当这条腿也不行，让调用方走原来的失败流程。
const SyncImageRelayTimeout = 4 * time.Minute

// BuildSyncImageRequestBody 把异步任务的请求体转换成同步 chat/completions 的请求体。
//
// 输入是客户提交给 /v1/videos 的原始 JSON，形如：
//
//	{"model":"gpt-image-2","prompt":"...","size":"720x1280","images":["https://..."]}
//
// 输出是 chat/completions 的 body：图片在前、文字在后（跟主站那条已验证的调法一致）。
func BuildSyncImageRequestBody(originalBody []byte, upstreamModel string) ([]byte, error) {
	var req map[string]any
	if err := common.Unmarshal(originalBody, &req); err != nil {
		return nil, fmt.Errorf("原始请求体不是合法 JSON: %w", err)
	}

	prompt := firstNonEmptyString(req, "prompt", "input", "text")
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("原始请求里没有提示词，无法转换成 chat 格式")
	}
	//: 尺寸不是 chat 格式的一等参数，只能并进提示词。不并的话出来的图会是默认比例，
	//: 对客户来说等于换了个尺寸——那比失败更糟，因为它看起来是成功的。
	if size := strings.TrimSpace(asString(req["size"])); size != "" {
		prompt = fmt.Sprintf("%s\n\n(image size: %s)", prompt, size)
	}

	content := make([]any, 0, 4)
	for _, u := range referenceImageURLs(req) {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": u},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})

	body := map[string]any{
		"model": upstreamModel,
		"messages": []any{
			map[string]any{"role": "user", "content": content},
		},
	}
	return common.Marshal(body)
}

// referenceImageURLs 从原始请求里把参考图地址抠出来。
// 兼容几种常见字段名——客户端和上游对这个字段的叫法一直不统一。
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
func ParseSyncImageResponse(body []byte) (*SyncImageResult, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := common.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON: %s", truncate(string(body), 200))
	}
	if resp.Error != nil && strings.TrimSpace(resp.Error.Message) != "" {
		return nil, fmt.Errorf("上游报错: %s", truncate(resp.Error.Message, 300))
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("上游响应里没有 choices: %s", truncate(string(body), 200))
	}

	text := flattenChatContent(resp.Choices[0].Message.Content)
	if url := firstMatch(markdownImageURLPattern, text); url != "" {
		return &SyncImageResult{ImageURL: url, RawBody: body}, nil
	}
	if url := firstMatch(bareImageURLPattern, text); url != "" {
		return &SyncImageResult{ImageURL: url, RawBody: body}, nil
	}
	//: 走到这里通常是内容审核 —— 上游返回了一段解释文字而不是图。
	//: 原样带回去让调用方去判，这里不猜。
	return nil, fmt.Errorf("上游没有返回图片，只回了文字: %s", truncate(strings.TrimSpace(text), 300))
}

// flattenChatContent 把 chat 响应的 content 拍平成文本。
// content 可能是字符串，也可能是 [{type,text},…] 这种数组。
func flattenChatContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s := asString(m["text"]); s != "" {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
			//: 有的上游把图片放在 image_url 结构里而不是 markdown 文本里。
			if iu, ok := m["image_url"].(map[string]any); ok {
				if s := asString(iu["url"]); s != "" {
					sb.WriteString(s)
					sb.WriteString("\n")
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// CallSyncImageRelay 同步调用一个只有 chat/completions 的出图上游。
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
	endpoint := baseURL + "/v1/chat/completions"

	callCtx, cancel := context.WithTimeout(ctx, SyncImageRelayTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
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

func firstMatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) > 1 {
		return m[1]
	}
	if len(m) == 1 {
		return m[0]
	}
	return ""
}
