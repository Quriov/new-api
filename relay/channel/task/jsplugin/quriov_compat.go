package jsplugin

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
)

// Quriov 改造 4：升级到 rc.40 后 /v1/videos 行为回到跟 rc.25 时代一致的两处垫片。
// 两处都是本地对照演练（同一脚本、同一 mock 上游，旧镜像 23/23 通过）实测出来的回归，详见 QURIOV.md。

// quriovSoraLegacyPassthrough：sora 插件接「没被它认领的型号」时，请求体原样透传。
//
// 背景：上游 rc.27 把 Go 的 sora 适配器换成了 JS 插件。旧适配器把客户的 JSON 原样转发
// （只换 model）；新插件只有在型号被它认领（meta.models = sora-2 / sora-2-pro）时才走
// openai_video 协议解码器（同样是原样透传），其余型号退回 ValidateBasicTaskRequest 的
// 固定结构体 —— aspect_ratio / image_size / resolution 这些不在结构体里的字段被悄悄丢掉。
// 我们卖的 gpt-image-2 / gpt-image-2.5-* / veo / omni 都不在它的认领表里，
// 于是客户要 16:9 拿到的是默认比例，2K 的参数覆盖也因为看不到 aspect_ratio 而补错尺寸。
//
// 做法：只在「sora 插件 + 旧式路由(没有 pinned endpoint) + JSON 请求体」时，把原始 JSON
// 对象作为 task_request 交给插件 —— 跟插件自己的 openai_video 解码器同一个语义。
// 其余插件、multipart 请求体一律不动。
func quriovSoraLegacyPassthrough(c *gin.Context, plugin *pluginruntime.LoadedPlugin) (map[string]any, bool) {
	if c == nil || c.Request == nil || plugin == nil || plugin.Meta.Key != "sora" {
		return nil, false
	}
	if _, pinned := c.Get(pluginruntime.ContextKeyPinnedEndpoint); pinned {
		return nil, false
	}
	if !strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "application/json") {
		return nil, false
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, false
	}
	raw, err := storage.Bytes()
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	var body map[string]any
	if err := common.Unmarshal(raw, &body); err != nil || body == nil {
		return nil, false
	}
	return body, true
}

// quriovPerCallPatched：TASK_PRICE_PATCH 里列名的型号 = 按次固定价（ModelPrice）。
//
// 这些型号的价格跟插件的用量 schema（秒数、尺寸枚举）无关，所以：
//   - 不拿插件的用量 schema 校验请求（sora 的 size 枚举只有 4 个视频尺寸，
//     gpt-image-2 的 1024x1024 会被 400 plugin_usage_invalid 拒掉）；
//   - 计价侧见 relay/relay_task.go 同名判断（不启用内置计费表达式、不做用量估算）。
func quriovPerCallPatched(modelNames ...string) bool {
	for _, name := range modelNames {
		if name != "" && common.StringsContains(constant.TaskPricePatches, name) {
			return true
		}
	}
	return false
}
