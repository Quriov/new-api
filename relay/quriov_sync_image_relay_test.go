package relay

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这些测试盯的是「异步请求 ↔ 同步上游」的协议转换。
//
// 为什么重要: 这条转换的存在意义, 是让唯一一家【跟主通道不同源】的上游能当备份腿。
// 转错了的后果不是报错, 是**出一张不对的图交给客户** —— 比失败更糟, 因为它看起来成功了。

func TestBuildSyncImageRequestBody_PutsImagesBeforeText(t *testing.T) {
	body, err := BuildSyncImageRequestBody(
		[]byte(`{"model":"gpt-image-2","prompt":"a red cat","images":["https://x/1.png","https://x/2.png"]}`),
		"gpt-image-2")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "gpt-image-2", got["model"])

	content := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	require.Len(t, content, 3, "两张参考图 + 一段文字")
	//: 顺序是图在前、文字在后 —— 跟主站那条已经验证过的调法一致。反了会影响出图。
	assert.Equal(t, "image_url", content[0].(map[string]any)["type"])
	assert.Equal(t, "image_url", content[1].(map[string]any)["type"])
	assert.Equal(t, "text", content[2].(map[string]any)["type"])
}

func TestBuildSyncImageRequestBody_CarriesSizeIntoThePrompt(t *testing.T) {
	body, err := BuildSyncImageRequestBody(
		[]byte(`{"model":"gpt-image-2","prompt":"a red cat","size":"720x1280"}`), "gpt-image-2")
	require.NoError(t, err)
	//: 尺寸不是 chat 格式的一等参数。不并进提示词的话出来的图是默认比例 ——
	//: 对客户来说等于换了个尺寸, 那比失败更糟, 因为它看起来是成功的。
	assert.Contains(t, string(body), "720x1280", "尺寸必须带过去，否则出图比例会悄悄变掉")
}

func TestBuildSyncImageRequestBody_UsesTheUpstreamModelName(t *testing.T) {
	body, err := BuildSyncImageRequestBody(
		[]byte(`{"model":"gpt-image-2","prompt":"x"}`), "gpt-image-2-vip")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "gpt-image-2-vip", got["model"],
		"必须用新渠道映射后的模型名，不能原样抄客户请求里那个")
}

func TestBuildSyncImageRequestBody_RejectsRequestWithoutPrompt(t *testing.T) {
	_, err := BuildSyncImageRequestBody([]byte(`{"model":"gpt-image-2","size":"720x1280"}`), "gpt-image-2")
	require.Error(t, err, "没有提示词就不该硬发出去 —— 上游会用空提示词出一张无关的图")
}

func TestBuildSyncImageRequestBody_WorksWithNoReferenceImages(t *testing.T) {
	body, err := BuildSyncImageRequestBody([]byte(`{"model":"gpt-image-2","prompt":"a red cat"}`), "gpt-image-2")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	content := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	require.Len(t, content, 1, "纯文生图: 只有一段文字")
}

// ── 响应解析 ──────────────────────────────────────────────────────────

func TestParseSyncImageResponse_ExtractsMarkdownImageURL(t *testing.T) {
	//: 这是同步上游的真实返回形状(见主站 image2_relay 的文档注释)。
	resp := []byte(`{"choices":[{"message":{"content":"![image](https://oss.filenest.top/uploads/abc.png)\n\n"}}]}`)
	got, err := ParseSyncImageResponse(resp)
	require.NoError(t, err)
	assert.Equal(t, "https://oss.filenest.top/uploads/abc.png", got.ImageURL)
}

func TestParseSyncImageResponse_HandlesImageBlockNotOnFirstLine(t *testing.T) {
	//: 有些提示词会带一段解释文字, 图片块在中间或末尾。
	resp := []byte(`{"choices":[{"message":{"content":"Here you go:\n\n![image](https://oss.example.top/u/x.png)\n\nEnjoy."}}]}`)
	got, err := ParseSyncImageResponse(resp)
	require.NoError(t, err)
	assert.Equal(t, "https://oss.example.top/u/x.png", got.ImageURL)
}

func TestParseSyncImageResponse_FallsBackToBareURL(t *testing.T) {
	resp := []byte(`{"choices":[{"message":{"content":"https://oss.example.top/u/y.jpg"}}]}`)
	got, err := ParseSyncImageResponse(resp)
	require.NoError(t, err)
	assert.Equal(t, "https://oss.example.top/u/y.jpg", got.ImageURL)
}

func TestParseSyncImageResponse_HandlesStructuredContentArray(t *testing.T) {
	resp := []byte(`{"choices":[{"message":{"content":[{"type":"text","text":"![image](https://o/z.png)"}]}}]}`)
	got, err := ParseSyncImageResponse(resp)
	require.NoError(t, err)
	assert.Equal(t, "https://o/z.png", got.ImageURL)
}

// ⭐ 反向: 上游只回了文字(通常是内容审核) —— 绝不能当成功。
func TestParseSyncImageResponse_TextOnlyReplyIsAnError(t *testing.T) {
	resp := []byte(`{"choices":[{"message":{"content":"I can't generate that image."}}]}`)
	_, err := ParseSyncImageResponse(resp)
	require.Error(t, err, "上游没给图却判成功 = 把一个空结果交给客户")
	assert.Contains(t, err.Error(), "没有返回图片")
}

func TestParseSyncImageResponse_UpstreamErrorIsSurfaced(t *testing.T) {
	resp := []byte(`{"error":{"message":"insufficient quota","code":"quota"}}`)
	_, err := ParseSyncImageResponse(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "insufficient quota", "上游的原话要带出来，否则排查时只能猜")
}

func TestParseSyncImageResponse_EmptyChoicesIsAnError(t *testing.T) {
	_, err := ParseSyncImageResponse([]byte(`{"choices":[]}`))
	require.Error(t, err)
}

func TestParseSyncImageResponse_GarbageIsAnError(t *testing.T) {
	_, err := ParseSyncImageResponse([]byte(`<html>502 Bad Gateway</html>`))
	require.Error(t, err, "上游挂了返回 HTML 时不能当成没图但成功")
}

// ⭐ 这条钉的是「判据不能恒真」: 拿一个明确该失败的输入喂进去必须失败，
// 拿一个明确该成功的喂进去必须成功。两个方向都在，解析器才不是摆设。
func TestParseSyncImageResponse_BothDirectionsWork(t *testing.T) {
	ok, err := ParseSyncImageResponse([]byte(`{"choices":[{"message":{"content":"![i](https://a/b.png)"}}]}`))
	require.NoError(t, err)
	require.NotEmpty(t, ok.ImageURL)

	_, err = ParseSyncImageResponse([]byte(`{"choices":[{"message":{"content":"nope"}}]}`))
	require.Error(t, err)
}

func TestSyncImageRelayTimeoutIsGenerousEnoughForRealUpstreams(t *testing.T) {
	//: 实测这类上游出一张图要 44–90 秒。超时设得比这短 = 这条腿基本用不上,
	//: 而且失败方式是静默的(看起来像上游慢)。
	assert.GreaterOrEqual(t, SyncImageRelayTimeout.Seconds(), 120.0,
		"同步出图上限不能低于 2 分钟，否则这条备份腿等于没有")
}

func TestMarkdownPatternDoesNotMatchPlainProse(t *testing.T) {
	//: 防止正则写太松, 把提示词里出现的普通链接当成结果图。
	_, err := ParseSyncImageResponse([]byte(
		`{"choices":[{"message":{"content":"参考 https://example.com/docs 里的说明"}}]}`))
	require.Error(t, err, "普通链接(非图片后缀、非 markdown 图片块)不该被当成出图结果")
}
