package relay

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这些测试盯的是「异步任务请求 ↔ 同步出图上游」的协议转换。
//
// ⚠⚠ 用的形状是 2026-08-24 **对真上游实测**出来的，不是照文档或照别处的写法抄的。
//     第一版照主站那套走 chat/completions，单测**全绿**，真打一次直接被拒：
//     "This model is not supported on the Chat Completions endpoint"。
//     ⇒ 单测只能证明"我实现的是我以为的协议"，证明不了"我以为的协议是对的"。
//     下面每条实测数据都标了出处。

// ── 请求转换 ──────────────────────────────────────────────────────────

func TestBuildSyncImageRequestBody_UsesTheImagesEndpointShape(t *testing.T) {
	body, err := BuildSyncImageRequestBody(
		[]byte(`{"model":"gpt-image-2","prompt":"a red apple","size":"720x1280"}`), "gpt-image-2")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "gpt-image-2", got["model"])
	assert.Equal(t, "a red apple", got["prompt"])
	assert.Equal(t, float64(1), got["n"])
	//: size 是这个端点的一等参数。实测(2026-08-24)传 720x1280 上游原样回显 size:"720x1280"。
	assert.Equal(t, "720x1280", got["size"])
	//: 反向: 不该再有 chat 格式的残留。
	assert.NotContains(t, got, "messages", "这个端点不吃 chat 格式 —— 实测会被拒")
}

func TestBuildSyncImageRequestBody_SizeStaysAParameterNotPromptText(t *testing.T) {
	body, err := BuildSyncImageRequestBody(
		[]byte(`{"model":"gpt-image-2","prompt":"a red apple","size":"1024x1024"}`), "gpt-image-2")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	//: 老版本把尺寸拼进提示词（chat 格式时代的将就做法）。现在端点原生支持，
	//: 再拼进去等于让模型把"image size: …"当成画面内容的一部分。
	assert.NotContains(t, got["prompt"], "image size", "尺寸不该再出现在提示词里")
	assert.NotContains(t, got["prompt"], "1024x1024")
}

func TestBuildSyncImageRequestBody_WorksWithoutSize(t *testing.T) {
	body, err := BuildSyncImageRequestBody([]byte(`{"model":"gpt-image-2","prompt":"x"}`), "gpt-image-2")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	_, hasSize := got["size"]
	assert.False(t, hasSize, "客户没指定尺寸时不该硬塞一个 —— 让上游用它自己的默认值")
}

func TestBuildSyncImageRequestBody_UsesTheUpstreamModelName(t *testing.T) {
	body, err := BuildSyncImageRequestBody([]byte(`{"model":"gpt-image-2","prompt":"x"}`), "gpt-image-2-vip")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "gpt-image-2-vip", got["model"],
		"要用新渠道映射后的模型名，不能原样抄客户请求里那个")
}

func TestBuildSyncImageRequestBody_RejectsRequestWithoutPrompt(t *testing.T) {
	_, err := BuildSyncImageRequestBody([]byte(`{"model":"gpt-image-2","size":"720x1280"}`), "gpt-image-2")
	require.Error(t, err, "没有提示词就不该硬发出去 —— 上游会出一张无关的图")
}

// ⭐⭐ 最要紧的一条：带参考图的请求必须【拒绝】，不能降级成纯文生图。
func TestBuildSyncImageRequestBody_RefusesRequestsWithReferenceImages(t *testing.T) {
	for _, raw := range []string{
		`{"model":"gpt-image-2","prompt":"x","images":["https://a/1.png"]}`,
		`{"model":"gpt-image-2","prompt":"x","image_urls":["https://a/1.png","https://a/2.png"]}`,
		`{"model":"gpt-image-2","prompt":"x","image":"https://a/1.png"}`,
	} {
		_, err := BuildSyncImageRequestBody([]byte(raw), "gpt-image-2")
		require.Error(t, err, "带参考图必须拒绝: %s", raw)
		assert.True(t, errors.Is(err, ErrSyncRelayNeedsReferenceImages),
			"要用可辨认的错误值，好让调用方区分「能力边界」和「上游挂了」")
	}
	//: 反向：纯文生图必须放行，否则这条腿等于永远不可用。
	_, err := BuildSyncImageRequestBody([]byte(`{"model":"gpt-image-2","prompt":"x"}`), "gpt-image-2")
	require.NoError(t, err)
}

// ── 响应解析 ──────────────────────────────────────────────────────────

// 真上游 2026-08-24 的实测响应（只删了 usage 细节）。
const realUpstreamResponse = `{"created":1787633527,"background":"opaque","data":[{"revised_prompt":"a single red apple, product photo","url":"https://r2.geeknow.top/uploads/image-generation-1787633527150940661.png"}],"output_format":"png","quality":"auto","size":"720x1280","usage":{"total_tokens":956}}`

func TestParseSyncImageResponse_AgainstRealUpstreamResponse(t *testing.T) {
	got, err := ParseSyncImageResponse([]byte(realUpstreamResponse))
	require.NoError(t, err)
	assert.Equal(t, "https://r2.geeknow.top/uploads/image-generation-1787633527150940661.png", got.ImageURL)
	assert.NotEmpty(t, got.RawBody, "原始响应要留着供排查")
}

func TestParseSyncImageResponse_UpstreamErrorIsSurfaced(t *testing.T) {
	//: 这条也是实测拿到的 —— 第一版走错端点时上游的原话。
	resp := []byte(`{"error":{"message":"This model is not supported on the Chat Completions endpoint","type":"invalid_request_error"}}`)
	_, err := ParseSyncImageResponse(resp)
	require.Error(t, err)
	//: ⚠ 这里必须断言【走的是哪个分支】，不能只断言消息里含上游的字眼 ——
	//   兜底分支("没有 data")会把整个原始响应体打进消息里，那段 JSON 本身就含
	//   "not supported"。第一版就是这么写的，变异("忽略上游报错")当场活了下来。
	assert.True(t, strings.HasPrefix(err.Error(), "上游报错: "),
		"必须走「上游明确报错」这个分支，而不是掉进「没有 data」的兜底。实际: %s", err.Error())
	assert.Contains(t, err.Error(), "not supported", "上游的原话要带出来，否则排查时只能猜")
}

func TestParseSyncImageResponse_EmptyDataIsAnError(t *testing.T) {
	_, err := ParseSyncImageResponse([]byte(`{"created":1,"data":[]}`))
	require.Error(t, err)
}

func TestParseSyncImageResponse_Base64OnlyIsRejectedNotFaked(t *testing.T) {
	//: 上游给内嵌 base64 时我们没有落盘的地方。明确拒绝, 不能假装成功
	//: (硬塞进 task.Data 会让那一行变成几 MB)。
	_, err := ParseSyncImageResponse([]byte(`{"data":[{"b64_json":"iVBORw0KGgo="}]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base64")
}

func TestParseSyncImageResponse_GarbageIsAnError(t *testing.T) {
	_, err := ParseSyncImageResponse([]byte(`<html>502 Bad Gateway</html>`))
	require.Error(t, err, "上游挂了返回 HTML 时不能当成没图但成功")
}

// ⭐ 钉「判据不能恒真」: 一个明确该成功、一个明确该失败，两个方向都在。
func TestParseSyncImageResponse_BothDirectionsWork(t *testing.T) {
	ok, err := ParseSyncImageResponse([]byte(realUpstreamResponse))
	require.NoError(t, err)
	require.NotEmpty(t, ok.ImageURL)

	_, err = ParseSyncImageResponse([]byte(`{"data":[{"url":""}]}`))
	require.Error(t, err, "url 是空串时不能当成功 —— 那会把一个空地址交给客户")
}

// ── 常量 ──────────────────────────────────────────────────────────────

func TestSyncImageRelayUsesTheEndpointThatActuallyWorks(t *testing.T) {
	//: 实测 /v1/chat/completions 被拒、/v1/images/generations 通(HTTP 200, 21-26 秒)。
	//: 改这个常量之前请先对真上游打一次 —— 单测证明不了协议对不对。
	assert.Equal(t, "/v1/images/generations", SyncImageRelayPath)
}

func TestSyncImageRelayTimeoutIsGenerousEnoughForRealUpstreams(t *testing.T) {
	//: 实测 21-26 秒。超时设得比这短 = 这条腿基本用不上，
	//: 而且失败方式是静默的(看起来像上游慢，实际是我们自己掐的)。
	assert.GreaterOrEqual(t, SyncImageRelayTimeout.Seconds(), 120.0,
		"同步出图上限不能低于 2 分钟，否则这条备份腿等于没有")
}
