package jsplugin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// QURIOV.md 改造 3：渠道「参数覆盖」作用于任务提交体。
//
// 场景：对外卖三个型号 img-x-1K / img-x-2K / img-x-4K，其中 2K、4K 在上游是
// 同一个型号 img-x-tiered，靠请求里的 image_size 选档。渠道用模型映射改名，
// 用参数覆盖按 original_model 补档位。
//
// 上游 rc.27 起 /v1/videos 由 sora JS 插件承接（旧的 Go 适配器 relay/channel/task/sora 已删），
// 所以这组测试直接拿【内置的 sora 插件】跑 —— 跟生产走的是同一段 JS。
const tierOverrideJSON = `{
  "operations": [
    {
      "path": "image_size", "mode": "set", "value": "2K", "keep_origin": true,
      "logic": "AND",
      "conditions": [
        {"path": "original_model", "mode": "full", "value": "img-x-2K"},
        {"path": "size", "mode": "prefix", "value": "", "invert": true, "pass_missing_key": true},
        {"path": "resolution", "mode": "prefix", "value": "", "invert": true, "pass_missing_key": true}
      ]
    },
    {
      "path": "image_size", "mode": "set", "value": "4K", "keep_origin": true,
      "logic": "AND",
      "conditions": [
        {"path": "original_model", "mode": "full", "value": "img-x-4K"},
        {"path": "size", "mode": "prefix", "value": "", "invert": true, "pass_missing_key": true},
        {"path": "resolution", "mode": "prefix", "value": "", "invert": true, "pass_missing_key": true}
      ]
    }
  ]
}`

func tierOverride(t *testing.T) map[string]any {
	t.Helper()
	m := map[string]any{}
	require.NoError(t, common.Unmarshal([]byte(tierOverrideJSON), &m))
	return m
}

func soraAdaptor(t *testing.T) *TaskAdaptor {
	t.Helper()
	source, err := plugins.Source("sora")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	return New(plugin)
}

func overrideInfo(origin, upstream string, override map[string]any) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		OriginModelName: origin,
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: "task_public"},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    "https://provider.example",
			ApiKey:            "sk-test",
			UpstreamModelName: upstream,
			IsModelMapped:     origin != upstream,
			ParamOverride:     override,
		},
	}
}

// buildSoraBody 走跟生产相同的两步：校验(插件 buildSubmitRequest) → 构建请求体。
func buildSoraBody(t *testing.T, request map[string]any, info *relaycommon.RelayInfo) (map[string]any, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	adaptor := soraAdaptor(t)
	adaptor.Init(info)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("task_request", request)
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
	body, err := adaptor.BuildRequestBody(c, info)
	if err != nil {
		return nil, err
	}
	sent, readErr := io.ReadAll(body)
	require.NoError(t, readErr)
	got := map[string]any{}
	require.NoError(t, common.Unmarshal(sent, &got))
	return got, nil
}

func TestParamOverride_InjectsTierForMappedModel(t *testing.T) {
	for origin, want := range map[string]string{"img-x-2K": "2K", "img-x-4K": "4K"} {
		got, err := buildSoraBody(t, map[string]any{"model": origin, "prompt": "a cat", "aspect_ratio": "16:9"},
			overrideInfo(origin, "img-x-tiered", tierOverride(t)))
		require.NoError(t, err)
		assert.Equal(t, "img-x-tiered", got["model"], origin)
		assert.Equal(t, want, got["image_size"], origin)
		assert.Equal(t, "16:9", got["aspect_ratio"], origin)
		assert.Equal(t, "a cat", got["prompt"], origin)
	}
}

// 客户自己给了档位 / 像素尺寸 → 以客户为准，不叠加。
func TestParamOverride_RespectsClientSuppliedTier(t *testing.T) {
	got, err := buildSoraBody(t, map[string]any{"model": "img-x-2K", "prompt": "x", "image_size": "4K"},
		overrideInfo("img-x-2K", "img-x-tiered", tierOverride(t)))
	require.NoError(t, err)
	assert.Equal(t, "4K", got["image_size"], "keep_origin 必须保住客户传的值")

	got, err = buildSoraBody(t, map[string]any{"model": "img-x-4K", "prompt": "x", "size": "1280x720"},
		overrideInfo("img-x-4K", "img-x-tiered", tierOverride(t)))
	require.NoError(t, err)
	assert.Equal(t, "1280x720", got["size"])
	_, has := got["image_size"]
	assert.False(t, has, "客户传了 size 就不能再补 image_size")

	got, err = buildSoraBody(t, map[string]any{"model": "img-x-4K", "prompt": "x", "resolution": "2K"},
		overrideInfo("img-x-4K", "img-x-tiered", tierOverride(t)))
	require.NoError(t, err)
	_, has = got["image_size"]
	assert.False(t, has, "客户传了 resolution 就不能再补 image_size")
}

// 同一渠道上的其它型号（没映射、名字不匹配）不能被这份配置碰到。
func TestParamOverride_LeavesOtherModelsUntouched(t *testing.T) {
	for _, m := range []string{"img-x-1K", "other-image-2K", "some-video"} {
		got, err := buildSoraBody(t, map[string]any{"model": m, "prompt": "x"}, overrideInfo(m, m, tierOverride(t)))
		require.NoError(t, err)
		_, has := got["image_size"]
		assert.False(t, has, m)
		assert.Equal(t, m, got["model"], m)
	}
}

// 回归守卫：没配参数覆盖的渠道（生产上绝大多数渠道），请求体只有 model 被换成上游名。
func TestParamOverride_NoOverrideKeepsPreviousBehaviour(t *testing.T) {
	got, err := buildSoraBody(t,
		map[string]any{"model": "img-x-2K", "prompt": "x", "metadata": map[string]any{"urls": []any{"https://a/1.png"}}},
		overrideInfo("img-x-2K", "img-x-tiered", nil))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"model":    "img-x-tiered",
		"prompt":   "x",
		"metadata": map[string]any{"urls": []any{"https://a/1.png"}},
	}, got)
}

// 渠道配置写成 return_error 时，提交在发往上游之前就失败（不会带着错的参数出门），
// 且错误前缀保持 apply_param_override_failed（运维手册按这个前缀认错误）。
func TestParamOverride_ReturnErrorFailsBeforeUpstream(t *testing.T) {
	override := map[string]any{}
	require.NoError(t, common.Unmarshal([]byte(`{"operations":[{"mode":"return_error","value":"blocked"}]}`), &override))

	_, err := buildSoraBody(t, map[string]any{"model": "img-x-2K", "prompt": "x"}, overrideInfo("img-x-2K", "img-x-tiered", override))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apply_param_override_failed")
}
