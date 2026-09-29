package jsplugin

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// QURIOV.md 改造 4：rc.40 的 sora 插件在两处跟旧 Go 适配器行为不同，这里钉住垫片。
// 两处都是本地对照演练实测出来的：同一脚本、同一 mock 上游，旧镜像全过、rc.40 原样失败。

// legacySoraSubmit 模拟旧式 /v1/videos 路由（型号没被插件认领 ⇒ 没有 pinned endpoint）：
// 请求体放在 body storage 里，由 ValidateRequestAndSetAction 自己解析。
func legacySoraSubmit(t *testing.T, raw string, origin string) (map[string]any, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	adaptor := soraAdaptor(t)
	info := overrideInfo(origin, origin, nil)
	adaptor.Init(info)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader([]byte(raw)))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
		return nil, fmt.Errorf("%s: %s", taskErr.Code, taskErr.Message)
	}
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	sent, err := io.ReadAll(body)
	require.NoError(t, err)
	got := map[string]any{}
	require.NoError(t, common.Unmarshal(sent, &got))
	return got, nil
}

func withTaskPricePatches(t *testing.T, models ...string) {
	t.Helper()
	orig := constant.TaskPricePatches
	constant.TaskPricePatches = models
	t.Cleanup(func() { constant.TaskPricePatches = orig })
}

// ⭐ 客户传的 aspect_ratio / image_size 这类「不在固定结构体里」的字段必须原样到上游。
// rc.40 原样：aspect_ratio 被丢，客户要 16:9 拿到默认比例；2K 的参数覆盖也因此补错尺寸。
func TestQuriovCompat_UnclaimedModelBodyPassesThrough(t *testing.T) {
	withTaskPricePatches(t, "gpt-image-2")
	got, err := legacySoraSubmit(t,
		`{"model":"gpt-image-2","prompt":"p","aspect_ratio":"9:16","image_size":"2K","images":["https://ref.example/a.png"],"n":1}`,
		"gpt-image-2")
	require.NoError(t, err)
	assert.Equal(t, "9:16", got["aspect_ratio"])
	assert.Equal(t, "2K", got["image_size"])
	assert.Equal(t, []any{"https://ref.example/a.png"}, got["images"])
	assert.EqualValues(t, 1, got["n"])
	assert.Equal(t, "gpt-image-2", got["model"])
}

// ⭐ 按次固定价的型号不拿 sora 的用量 schema 校验：它的 size 只认 4 个视频尺寸。
// rc.40 原样：gpt-image-2 传 size=1024x1024 直接 400 plugin_usage_invalid。
func TestQuriovCompat_PerCallModelSkipsSoraUsageSchema(t *testing.T) {
	withTaskPricePatches(t, "gpt-image-2")
	got, err := legacySoraSubmit(t, `{"model":"gpt-image-2","prompt":"p","size":"1024x1024"}`, "gpt-image-2")
	require.NoError(t, err)
	assert.Equal(t, "1024x1024", got["size"])
}

// 反向：没列进 TASK_PRICE_PATCH 的型号（比如真 sora-2）照旧受上游用量校验 ——
// 垫片只放行我们自己按次卖的型号，不是把校验整个关掉。
func TestQuriovCompat_UnpatchedModelStillValidated(t *testing.T) {
	withTaskPricePatches(t, "gpt-image-2")
	_, err := legacySoraSubmit(t, `{"model":"sora-2","prompt":"p","size":"1024x1024"}`, "sora-2")
	require.Error(t, err, "sora-2 的非法尺寸必须照旧被拒")
}

// 基本校验照做：没有 prompt 仍然拒绝（透传不等于放弃校验）。
func TestQuriovCompat_PassthroughKeepsBasicValidation(t *testing.T) {
	withTaskPricePatches(t, "gpt-image-2")
	_, err := legacySoraSubmit(t, `{"model":"gpt-image-2","aspect_ratio":"1:1"}`, "gpt-image-2")
	require.Error(t, err)
}

// 只作用于 sora 插件的旧式路由：带 pinned endpoint（协议路由）时不介入。
func TestQuriovCompat_PassthroughOnlyForLegacySoraRoute(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader([]byte(`{"prompt":"p"}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })

	adaptor := soraAdaptor(t)
	_, ok := quriovSoraLegacyPassthrough(c, adaptor.plugin)
	assert.True(t, ok)

	c.Set("task_plugin_pinned_endpoint", struct{}{})
	_, ok = quriovSoraLegacyPassthrough(c, adaptor.plugin)
	assert.False(t, ok, "协议路由自己有解码器，垫片不该插手")

	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader([]byte(`{"prompt":"p"}`)))
	c2.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c2) })
	_, ok = quriovSoraLegacyPassthrough(c2, nil)
	assert.False(t, ok)
}
