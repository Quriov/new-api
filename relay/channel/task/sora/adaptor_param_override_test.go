package sora

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// QURIOV.md 改造 3：渠道「参数覆盖」作用于任务提交体。
//
// 场景：对外卖三个型号 img-x-1K / img-x-2K / img-x-4K，其中 2K、4K 在上游是
// 同一个型号 img-x-tiered，靠请求里的 image_size 选档。渠道用模型映射改名，
// 用参数覆盖按 original_model 补档位。下面这份配置就是生产要填的那种形状
// （型号名换成了中性的占位名）。
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

func tierOverride(t *testing.T) map[string]interface{} {
	t.Helper()
	m := map[string]interface{}{}
	require.NoError(t, common.Unmarshal([]byte(tierOverrideJSON), &m))
	return m
}

func buildJSONBody(t *testing.T, raw string, info *relaycommon.RelayInfo) map[string]interface{} {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader([]byte(raw)))
	c.Request.Header.Set("Content-Type", "application/json")
	defer common.CleanupBodyStorage(c)

	body, err := (&TaskAdaptor{}).BuildRequestBody(c, info)
	require.NoError(t, err)
	sent, err := io.ReadAll(body)
	require.NoError(t, err)
	got := map[string]interface{}{}
	require.NoError(t, common.Unmarshal(sent, &got))
	return got
}

func infoFor(origin, upstream string, override map[string]interface{}) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		OriginModelName: origin,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: upstream,
			IsModelMapped:     origin != upstream,
			ParamOverride:     override,
		},
	}
}

func TestParamOverride_InjectsTierForMappedModel(t *testing.T) {
	for origin, want := range map[string]string{"img-x-2K": "2K", "img-x-4K": "4K"} {
		got := buildJSONBody(t, `{"model":"`+origin+`","prompt":"a cat","aspect_ratio":"16:9"}`,
			infoFor(origin, "img-x-tiered", tierOverride(t)))
		assert.Equal(t, "img-x-tiered", got["model"], origin)
		assert.Equal(t, want, got["image_size"], origin)
		assert.Equal(t, "16:9", got["aspect_ratio"], origin)
		assert.Equal(t, "a cat", got["prompt"], origin)
	}
}

// 客户自己给了档位 / 像素尺寸 → 以客户为准，不叠加（上游文档：size 与 aspect_ratio+image_size 二选一）。
func TestParamOverride_RespectsClientSuppliedTier(t *testing.T) {
	got := buildJSONBody(t, `{"model":"img-x-2K","prompt":"x","image_size":"4K"}`,
		infoFor("img-x-2K", "img-x-tiered", tierOverride(t)))
	assert.Equal(t, "4K", got["image_size"], "keep_origin 必须保住客户传的值")

	got = buildJSONBody(t, `{"model":"img-x-4K","prompt":"x","size":"1280x720"}`,
		infoFor("img-x-4K", "img-x-tiered", tierOverride(t)))
	assert.Equal(t, "1280x720", got["size"])
	_, has := got["image_size"]
	assert.False(t, has, "客户传了 size 就不能再补 image_size")

	got = buildJSONBody(t, `{"model":"img-x-4K","prompt":"x","resolution":"2K"}`,
		infoFor("img-x-4K", "img-x-tiered", tierOverride(t)))
	_, has = got["image_size"]
	assert.False(t, has, "客户传了 resolution 就不能再补 image_size")
}

// 同一渠道上的其它型号（没映射、名字不匹配）不能被这份配置碰到。
func TestParamOverride_LeavesOtherModelsUntouched(t *testing.T) {
	for _, m := range []string{"img-x-1K", "other-image-2K", "some-video"} {
		got := buildJSONBody(t, `{"model":"`+m+`","prompt":"x"}`, infoFor(m, m, tierOverride(t)))
		_, has := got["image_size"]
		assert.False(t, has, m)
		assert.Equal(t, m, got["model"], m)
	}
}

// 回归守卫：没配参数覆盖的渠道（生产上所有现存渠道），行为与改造前完全一致。
func TestParamOverride_NoOverrideKeepsPreviousBehaviour(t *testing.T) {
	got := buildJSONBody(t, `{"model":"img-x-2K","prompt":"x","metadata":{"urls":["https://a/1.png"]}}`,
		infoFor("img-x-2K", "img-x-tiered", nil))
	assert.Equal(t, map[string]interface{}{
		"model":    "img-x-tiered",
		"prompt":   "x",
		"metadata": map[string]interface{}{"urls": []interface{}{"https://a/1.png"}},
	}, got)
}

// 渠道配置写成 return_error 时，提交在发往上游之前就失败（不会带着错的参数出门）。
func TestParamOverride_ReturnErrorFailsBeforeUpstream(t *testing.T) {
	override := map[string]interface{}{}
	require.NoError(t, common.Unmarshal([]byte(`{"operations":[{"mode":"return_error","value":"blocked"}]}`), &override))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader([]byte(`{"model":"img-x-2K","prompt":"x"}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	defer common.CleanupBodyStorage(c)

	_, err := (&TaskAdaptor{}).BuildRequestBody(c, infoFor("img-x-2K", "img-x-tiered", override))
	require.Error(t, err)
}
