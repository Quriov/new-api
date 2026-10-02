package jsplugin

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plugins/uploads/async-image-relay/plugin.js 的测试：从后台上传的任务插件（不随二进制内置），
// 把 /v1/videos 上的图片任务改发到上游的「异步出图」接口。这里全部用本地假上游（httptest），不连任何真上游。
//
// 走的是跟生产同一条适配器链：协议解码 → ValidateRequestAndSetAction（插件 buildSubmitRequest）→
// BuildRequestBody → DoRequest → ParseResponse → FetchTask → ParseTaskResult → ConvertToOpenAIVideo。

const asyncImagePluginPath = "../../../../plugins/uploads/async-image-relay/plugin.js"

const (
	asyncSubmitPath = "/v1/images/generations/async"
	asyncQueryPath  = "/v1/images/generations/async/"
)

func asyncImageSource(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile(asyncImagePluginPath)
	require.NoError(t, err)
	return string(source)
}

// 跟后台「上传插件」同一条注册路径（Register，不是内置插件用的 RegisterFactory）。
func asyncImagePlugin(t *testing.T) (*pluginruntime.Registry, *pluginruntime.LoadedPlugin) {
	t.Helper()
	registry := pluginruntime.NewRegistry()
	plugin, err := registry.Register(asyncImageSource(t), pluginruntime.Options{})
	require.NoError(t, err)
	return registry, plugin
}

// fakeAsyncImageUpstream 是假上游：记下每次提交，查询按 polls 里排好的响应依次回（回完停在最后一个）。
type fakeAsyncImageUpstream struct {
	mu           sync.Mutex
	submits      []map[string]any
	submitAuth   []string
	queries      []string
	submitStatus int
	submitBody   string
	polls        []string
	pollStatus   int
	server       *httptest.Server
}

func newFakeAsyncImageUpstream(t *testing.T) *fakeAsyncImageUpstream {
	t.Helper()
	upstream := &fakeAsyncImageUpstream{
		submitStatus: http.StatusOK,
		submitBody:   `{"id":"task_img_1","task_id":"task_img_1","object":"image.generation.task","model":"m","status":"queued","created_at":1735689600}`,
		pollStatus:   http.StatusOK,
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == asyncSubmitPath:
			raw, _ := io.ReadAll(r.Body)
			body := map[string]any{}
			_ = common.Unmarshal(raw, &body)
			upstream.submits = append(upstream.submits, body)
			upstream.submitAuth = append(upstream.submitAuth, r.Header.Get("Authorization"))
			w.WriteHeader(upstream.submitStatus)
			_, _ = w.Write([]byte(upstream.submitBody))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, asyncQueryPath):
			upstream.queries = append(upstream.queries, strings.TrimPrefix(r.URL.Path, asyncQueryPath))
			index := len(upstream.queries) - 1
			if index >= len(upstream.polls) {
				index = len(upstream.polls) - 1
			}
			w.WriteHeader(upstream.pollStatus)
			_, _ = w.Write([]byte(upstream.polls[index]))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *fakeAsyncImageUpstream) submitCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.submits)
}

type asyncImageRun struct {
	adaptor *TaskAdaptor
	info    *relaycommon.RelayInfo
	c       *gin.Context
}

// asyncImageDecode 调插件的 openai_video 解码器（/v1/videos 进来的第一步）。
func asyncImageDecode(t *testing.T, plugin *pluginruntime.LoadedPlugin, modelName string, request map[string]any) (map[string]any, error) {
	t.Helper()
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"protocol": "openai_video", "operation": "create", "model": modelName, "stream": false,
		"body": map[string]any{"kind": "json", "value": request},
	})
	if err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	intent := map[string]any{}
	require.NoError(t, common.Unmarshal(encoded, &intent))
	return intent, nil
}

// asyncImageValidate 走到「校验并构建提交」为止，返回适配器和校验结果（还没发任何上游请求）。
func asyncImageValidate(t *testing.T, baseURL, origin, upstream string, request map[string]any) (*asyncImageRun, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	_, plugin := asyncImagePlugin(t)
	intent, err := asyncImageDecode(t, plugin, origin, request)
	if err != nil {
		return nil, err
	}
	adaptor := New(plugin)
	info := overrideInfo(origin, upstream, nil)
	info.ChannelBaseUrl = baseURL
	adaptor.Init(info)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("task_request", intent["requestBody"])
	if action, ok := intent["action"].(string); ok {
		info.Action = action
	}
	if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
		return nil, fmt.Errorf("%s: %s", taskErr.Code, taskErr.Message)
	}
	return &asyncImageRun{adaptor: adaptor, info: info, c: c}, nil
}

// submit 把请求真发给假上游并解析响应，返回落库时会存的那条任务（上游任务号 + 插件状态）。
func (run *asyncImageRun) submit(t *testing.T) (*model.Task, error) {
	t.Helper()
	body, err := run.adaptor.BuildRequestBody(run.c, run.info)
	require.NoError(t, err)
	_, err = run.adaptor.BuildRequestURL(run.info)
	require.NoError(t, err)
	resp, err := run.adaptor.DoRequest(run.c, run.info, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	// 跟 RelayTaskSubmit 第 9 步同一条规则：非 2xx 在调插件之前就判提交失败，原样带上游的状态码和正文。
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fail_to_fetch_task %d: %s", resp.StatusCode, raw)
	}
	parsed, taskErr := run.adaptor.ParseResponse(run.c, resp, run.info)
	if taskErr != nil {
		return nil, fmt.Errorf("%s: %s", taskErr.Code, taskErr.Message)
	}
	return &model.Task{
		TaskID:      "task_public",
		Action:      run.info.Action,
		Data:        parsed.TaskData,
		Properties:  model.Properties{OriginModelName: run.info.OriginModelName, UpstreamModelName: run.info.UpstreamModelName},
		PrivateData: model.TaskPrivateData{UpstreamTaskID: parsed.UpstreamTaskID, PluginState: parsed.PluginState},
	}, nil
}

// poll 查一次假上游，像后台轮询那样把结果写回任务（状态 / 失败原因 / 最新快照）。
func (run *asyncImageRun) poll(t *testing.T, task *model.Task) *relaycommon.TaskInfo {
	t.Helper()
	resp, err := run.adaptor.FetchTask(run.info.ChannelBaseUrl, run.info.ApiKey, task, "")
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	result, err := run.adaptor.ParseTaskResult(task, resp, raw)
	require.NoError(t, err)
	task.Data = raw
	task.Status = model.TaskStatus(result.Status)
	task.FailReason = result.Reason
	return result
}

func (run *asyncImageRun) render(t *testing.T, task *model.Task) map[string]any {
	t.Helper()
	encoded, err := run.adaptor.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	rendered := map[string]any{}
	require.NoError(t, common.Unmarshal(encoded, &rendered))
	return rendered
}

func setAsyncImageSubmittedAt(t *testing.T, task *model.Task, submittedAt int64) {
	t.Helper()
	state := map[string]any{}
	require.NoError(t, common.Unmarshal(task.PrivateData.PluginState, &state))
	state["submittedAt"] = submittedAt
	encoded, err := common.Marshal(state)
	require.NoError(t, err)
	task.PrivateData.PluginState = encoded
}

// ① 提交成功 → 轮询 → 完成取图。
func TestAsyncImagePlugin_SubmitPollComplete(t *testing.T) {
	upstream := newFakeAsyncImageUpstream(t)
	upstream.polls = []string{
		`{"id":"task_img_1","task_id":"task_img_1","object":"image.generation.task","status":"queued","created_at":1735689600}`,
		`{"id":"task_img_1","task_id":"task_img_1","object":"image.generation.task","status":"in_progress","progress":"10%","created_at":1735689600}`,
		`{"created":1735689700,"data":[{"url":"https://cdn.example/out/task_img_1.png","revised_prompt":"p"}],"size":"2048x1152","quality":"high","usage":{"total_tokens":211}}`,
	}
	run, err := asyncImageValidate(t, upstream.server.URL, "gpt-image-2.5-2K", "gpt-image-2.5-2K", map[string]any{
		"model": "gpt-image-2.5-2K", "prompt": "  a red mug  ", "aspect_ratio": "16:9", "quality": "high",
		"images": []any{"https://ref.example/a.png", "https://ref.example/b.png"},
	})
	require.NoError(t, err)
	assert.Equal(t, "image_to_video", run.info.Action)
	task, err := run.submit(t)
	require.NoError(t, err)

	require.Equal(t, 1, upstream.submitCount())
	assert.Equal(t, "Bearer sk-test", upstream.submitAuth[0])
	assert.Equal(t, map[string]any{
		"model": "gpt-image-2.5-flare", "prompt": "a red mug", "n": float64(1), "size": "2048x1152",
		"response_format": "url", "watermark": false, "quality": "high",
		"image": []any{"https://ref.example/a.png", "https://ref.example/b.png"},
	}, upstream.submits[0], "上游只该收到它文档里的字段：比例已换成像素，参考图字段名是 image")
	assert.Equal(t, "task_img_1", task.PrivateData.UpstreamTaskID)

	first := run.poll(t, task)
	assert.Equal(t, "QUEUED", first.Status)
	second := run.poll(t, task)
	assert.Equal(t, "IN_PROGRESS", second.Status)
	assert.Equal(t, "10%", second.Progress)
	third := run.poll(t, task)
	assert.Equal(t, "SUCCESS", third.Status)
	assert.Equal(t, "https://cdn.example/out/task_img_1.png", third.Url)
	assert.Equal(t, []string{"task_img_1", "task_img_1", "task_img_1"}, upstream.queries)

	rendered := run.render(t, task)
	assert.Equal(t, "task_public", rendered["id"])
	assert.Equal(t, "completed", rendered["status"])
	assert.Equal(t, "gpt-image-2.5-2K", rendered["model"], "客户看到的是对外型号名，不是上游名")
	assert.Equal(t, "https://cdn.example/out/task_img_1.png", rendered["url"])
	assert.Equal(t, "https://cdn.example/out/task_img_1.png", rendered["image_url"])
	assert.Equal(t, "2048x1152", rendered["size"])
	assert.Equal(t, []any{map[string]any{"url": "https://cdn.example/out/task_img_1.png"}}, rendered["data"])
	assert.NotContains(t, rendered, "usage", "上游的 token 用量不外露")
	assert.NotContains(t, rendered, "error")
}

// 提交响应里三种任务号写法（id / task_id / taskId）都认。
func TestAsyncImagePlugin_SubmitAcceptsEveryDocumentedTaskIDField(t *testing.T) {
	for _, body := range []string{`{"id":"t-1","status":"queued"}`, `{"task_id":"t-1"}`, `{"taskId":"t-1","status":"queued"}`} {
		upstream := newFakeAsyncImageUpstream(t)
		upstream.submitBody = body
		run, err := asyncImageValidate(t, upstream.server.URL, "gpt-image-2", "gpt-image-2", map[string]any{"prompt": "p"})
		require.NoError(t, err)
		task, err := run.submit(t)
		require.NoError(t, err, body)
		assert.Equal(t, "t-1", task.PrivateData.UpstreamTaskID, body)
	}
}

// ② 上游提交被拒：不产生任务，原因可读。
func TestAsyncImagePlugin_SubmitRejectedByUpstream(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"HTTP 400 带错误对象", http.StatusBadRequest, `{"error":{"message":"prompt is required","type":"invalid_request_error","param":"prompt","code":"invalid_request"}}`, "prompt is required"},
		{"HTTP 503", http.StatusServiceUnavailable, `{"error":{"message":"account queue is full","code":"account_queue_full"}}`, "account queue is full"},
		{"HTTP 200 但正文是错误", http.StatusOK, `{"error":{"message":"model is not available","code":"model_not_found"}}`, "upstream rejected the submission: model is not available (model_not_found)"},
		{"HTTP 200 但提交即失败", http.StatusOK, `{"id":"t-1","status":"failed"}`, "upstream rejected the submission: task failed"},
		{"HTTP 200 但没有任务号", http.StatusOK, `{"object":"image.generation.task","status":"queued"}`, "returned no task id"},
		{"HTTP 200 但不是 JSON", http.StatusOK, `<html>bad gateway</html>`, "non-JSON submit response"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newFakeAsyncImageUpstream(t)
			upstream.submitStatus = testCase.status
			upstream.submitBody = testCase.body
			run, err := asyncImageValidate(t, upstream.server.URL, "gpt-image-2.5-1K", "gpt-image-2.5-1K", map[string]any{"prompt": "p", "aspect_ratio": "1:1"})
			require.NoError(t, err)
			task, err := run.submit(t)
			require.Error(t, err)
			assert.Nil(t, task, "提交被拒不该产生任务")
			assert.Contains(t, err.Error(), testCase.want)
			assert.Empty(t, upstream.queries, "被拒的提交不该去轮询")
		})
	}
}

// ③ 上游任务失败 / 被取消：任务失败，带上游给的原因。
func TestAsyncImagePlugin_UpstreamTaskFailed(t *testing.T) {
	cases := []struct {
		name string
		poll string
		want string
	}{
		{"failed 带错误对象", `{"id":"task_img_1","object":"image.generation.task","status":"failed","progress":"10%","error":{"message":"Image generation failed","code":"generation_failed"}}`, "upstream task failed: Image generation failed (generation_failed)"},
		{"failed 不带原因", `{"id":"task_img_1","status":"failed"}`, "upstream task failed: no reason given"},
		{"cancelled", `{"id":"task_img_1","status":"cancelled"}`, "upstream cancelled the task"},
		{"completed 但没有图片地址", `{"created":1,"data":[{"b64_json":"aGVsbG8="}],"size":"1024x1024"}`, "upstream finished the task without an image URL"},
		{"completed 但 data 为空", `{"status":"completed","data":[]}`, "upstream finished the task without an image URL"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newFakeAsyncImageUpstream(t)
			upstream.polls = []string{testCase.poll}
			run, err := asyncImageValidate(t, upstream.server.URL, "gpt-image-2.5-1K", "gpt-image-2.5-1K", map[string]any{"prompt": "p"})
			require.NoError(t, err)
			task, err := run.submit(t)
			require.NoError(t, err)
			result := run.poll(t, task)
			assert.Equal(t, "FAILURE", result.Status)
			assert.Equal(t, testCase.want, result.Reason)
			assert.Empty(t, result.Url)

			rendered := run.render(t, task)
			assert.Equal(t, "failed", rendered["status"])
			assert.Equal(t, map[string]any{"code": "image_generation_failed", "message": testCase.want}, rendered["error"])
			assert.NotContains(t, rendered, "url")
		})
	}
}

// ④ 轮询超时：超过插件自己的期限还没出图 ⇒ 判失败（不等宿主 24 小时的总超时）。
func TestAsyncImagePlugin_PollDeadline(t *testing.T) {
	upstream := newFakeAsyncImageUpstream(t)
	upstream.polls = []string{`{"id":"task_img_1","status":"in_progress","progress":"40%"}`}
	run, err := asyncImageValidate(t, upstream.server.URL, "gpt-image-2-4K", "gpt-image-2-4K", map[string]any{"prompt": "p"})
	require.NoError(t, err)
	task, err := run.submit(t)
	require.NoError(t, err)

	result := run.poll(t, task)
	assert.Equal(t, "IN_PROGRESS", result.Status, "期限内照常等")
	assert.Equal(t, "40%", result.Progress)

	setAsyncImageSubmittedAt(t, task, time.Now().Unix()-590)
	assert.Equal(t, "IN_PROGRESS", run.poll(t, task).Status, "还差 10 秒，不该提前判")

	setAsyncImageSubmittedAt(t, task, time.Now().Unix()-601)
	result = run.poll(t, task)
	assert.Equal(t, "FAILURE", result.Status)
	assert.Equal(t, "upstream did not finish within 600 seconds (last status in_progress)", result.Reason)
	rendered := run.render(t, task)
	assert.Equal(t, "failed", rendered["status"])

	// 过了期限但这一次查到的正好是成品：以成品为准，不白扔一张已经出好的图。
	upstream.polls = []string{`{"created":1,"data":[{"url":"https://cdn.example/late.png"}],"size":"2880x2880"}`}
	result = run.poll(t, task)
	assert.Equal(t, "SUCCESS", result.Status)
}

// 认不出的响应不许当成「还在跑」：回 UNKNOWN，由宿主按连续轮询失败计数；过了期限则直接判失败。
func TestAsyncImagePlugin_UnrecognizedPollIsNotTreatedAsProgress(t *testing.T) {
	upstream := newFakeAsyncImageUpstream(t)
	upstream.polls = []string{`{"id":"task_img_1","status":"weird_state"}`}
	run, err := asyncImageValidate(t, upstream.server.URL, "gpt-image-2", "gpt-image-2", map[string]any{"prompt": "p"})
	require.NoError(t, err)
	task, err := run.submit(t)
	require.NoError(t, err)
	result := run.poll(t, task)
	assert.Equal(t, "UNKNOWN", result.Status)
	assert.Contains(t, result.Reason, "weird_state")

	// 宿主对 400 这类 4xx 仍会调插件：带错误对象的 400 不是「排队中」。
	upstream.pollStatus = http.StatusBadRequest
	upstream.polls = []string{`{"error":{"message":"bad task id","code":"invalid_request"}}`}
	result = run.poll(t, task)
	assert.Equal(t, "UNKNOWN", result.Status)
	assert.Equal(t, "bad task id (invalid_request)", result.Reason)

	setAsyncImageSubmittedAt(t, task, time.Now().Unix()-601)
	result = run.poll(t, task)
	assert.Equal(t, "FAILURE", result.Status)
	assert.Equal(t, "upstream did not finish within 600 seconds (bad task id (invalid_request))", result.Reason)
}

// ⑤ 不支持的比例 / 尺寸 / 档位 / 张数：发上游之前就拒，上游一次请求都收不到。
func TestAsyncImagePlugin_UnsupportedRequestRejectedBeforeUpstream(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		request map[string]any
		want    string
	}{
		{"表外比例", "gpt-image-2.5-2K", map[string]any{"prompt": "p", "aspect_ratio": "7:3"}, "aspect_ratio 7:3 is not supported by model gpt-image-2.5-2K"},
		{"metadata 里的表外比例", "gpt-image-2.5-1K", map[string]any{"prompt": "p", "metadata": map[string]any{"aspect_ratio": "1:3"}}, "aspect_ratio 1:3 is not supported"},
		{"2.5 以外的型号不收只在 2.5 上实测过的比例", "gpt-image-2-2K", map[string]any{"prompt": "p", "aspect_ratio": "21:9"}, "aspect_ratio 21:9 is not supported by model gpt-image-2-2K"},
		{"拿 1K 型号要 4K 像素", "gpt-image-2.5-1K", map[string]any{"prompt": "p", "size": "3840x2160"}, "size 3840x2160 is not supported by model gpt-image-2.5-1K"},
		{"档位参数跟型号不一致", "gpt-image-2.5-1K", map[string]any{"prompt": "p", "image_size": "4K"}, "image_size 4K does not match model gpt-image-2.5-1K (1K)"},
		{"resolution 跟型号不一致", "gpt-image-2", map[string]any{"prompt": "p", "resolution": "2K"}, "resolution 2K does not match model gpt-image-2 (1K)"},
		{"一次要多张", "gpt-image-2", map[string]any{"prompt": "p", "n": 4}, "n must be 1"},
		{"没有 prompt", "gpt-image-2", map[string]any{"aspect_ratio": "1:1"}, "field prompt is required"},
		{"quality 不在该型号的取值里", "gpt-image-2", map[string]any{"prompt": "p", "quality": "xhigh"}, "quality xhigh is not supported by model gpt-image-2"},
		{"参考图是 base64", "gpt-image-2.5-1K", map[string]any{"prompt": "p", "images": []any{"data:image/png;base64,AAAA"}}, "reference images must be public http(s) URLs"},
		{"参考图是裸 base64", "gpt-image-2.5-1K", map[string]any{"prompt": "p", "image": "iVBORw0KGgoAAAANSUhEUgAA"}, "reference images must be public http(s) URLs"},
		{"参考图超过 8 张", "gpt-image-2.5-1K", map[string]any{"prompt": "p", "images": []any{
			"https://r.example/1.png", "https://r.example/2.png", "https://r.example/3.png", "https://r.example/4.png", "https://r.example/5.png",
			"https://r.example/6.png", "https://r.example/7.png", "https://r.example/8.png", "https://r.example/9.png"}}, "at most 8 reference images"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newFakeAsyncImageUpstream(t)
			_, plugin := asyncImagePlugin(t)

			// 协议解码器先拒（客户拿到的就是这句）。
			_, err := asyncImageDecode(t, plugin, testCase.model, testCase.request)
			require.ErrorContains(t, err, testCase.want)

			// 换渠道重投不经过解码器，直接把留存的原始请求交给 buildSubmitRequest：那里也必须拒。
			gin.SetMode(gin.TestMode)
			adaptor := New(plugin)
			info := overrideInfo(testCase.model, testCase.model, nil)
			info.ChannelBaseUrl = upstream.server.URL
			adaptor.Init(info)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("task_request", testCase.request)
			taskErr := adaptor.ValidateRequestAndSetAction(c, info)
			require.NotNil(t, taskErr)
			assert.Equal(t, "plugin_request_invalid", taskErr.Code)
			assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
			assert.Contains(t, taskErr.Message, testCase.want)
			assert.Zero(t, upstream.submitCount(), "被拒的请求不该发给上游")
		})
	}
}

// 文件上传（multipart）没法变成公网地址：解码阶段就拒。
func TestAsyncImagePlugin_MultipartRejected(t *testing.T) {
	_, plugin := asyncImagePlugin(t)
	_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "gpt-image-2",
		"body": map[string]any{"kind": "multipart", "fields": map[string]any{"prompt": []any{"p"}},
			"files": []any{map[string]any{"ref": "request_file:input_reference", "field": "input_reference", "filename": "a.png", "mimeType": "image/png", "size": 10}}},
	})
	require.ErrorContains(t, err, "JSON body required")
}

// 像素不够档位：上游回报的尺寸低于该档门槛 ⇒ 任务失败（失败不扣费由宿主负责），而且不把那张小图的地址交出去。
func TestAsyncImagePlugin_DeliveredSizeBelowTierFails(t *testing.T) {
	cases := []struct {
		name  string
		model string
		poll  string
		want  string
		ok    bool
	}{
		{"2K 退成 1K", "gpt-image-2.5-2K", `{"data":[{"url":"https://cdn.example/small.png"}],"size":"1254x1254"}`, "upstream returned a 1254x1254 image, below the 2K tier (long edge of at least 2000 px)", false},
		{"4K 只给了 2K", "gpt-image-2.5-4K", `{"data":[{"url":"https://cdn.example/small.png"}],"size":"2048x2048"}`, "upstream returned a 2048x2048 image, below the 4K tier (long edge of at least 2800 px)", false},
		{"4K 的 1:1 是 2880", "gpt-image-2.5-4K", `{"data":[{"url":"https://cdn.example/ok.png"}],"size":"2880x2880"}`, "", true},
		{"2K 竖图按长边算", "gpt-image-2-2K", `{"data":[{"url":"https://cdn.example/ok.png"}],"size":"1152x2048"}`, "", true},
		{"2K 没回报尺寸：没法确认，按失败", "gpt-image-2.5-2K", `{"data":[{"url":"https://cdn.example/unknown.png"}]}`, "upstream result did not report the image size, so the 2K tier cannot be confirmed", false},
		{"2K 回报的不是像素", "gpt-image-2.5-2K", `{"data":[{"url":"https://cdn.example/unknown.png"}],"size":"2K"}`, "upstream result did not report the image size, so the 2K tier cannot be confirmed", false},
		{"1K 没回报尺寸：最低档，放行", "gpt-image-2.5-1K", `{"data":[{"url":"https://cdn.example/ok.png"}]}`, "", true},
		{"1K 回报的尺寸太小", "gpt-image-2", `{"data":[{"url":"https://cdn.example/tiny.png"}],"size":"512x512"}`, "upstream returned a 512x512 image, below the 1K tier (long edge of at least 1000 px)", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newFakeAsyncImageUpstream(t)
			upstream.polls = []string{testCase.poll}
			run, err := asyncImageValidate(t, upstream.server.URL, testCase.model, testCase.model, map[string]any{"prompt": "p"})
			require.NoError(t, err)
			task, err := run.submit(t)
			require.NoError(t, err)
			result := run.poll(t, task)
			rendered := run.render(t, task)
			if testCase.ok {
				assert.Equal(t, "SUCCESS", result.Status)
				assert.NotEmpty(t, rendered["url"])
				return
			}
			assert.Equal(t, "FAILURE", result.Status)
			assert.Equal(t, testCase.want, result.Reason)
			assert.Empty(t, result.Url)
			assert.Equal(t, "failed", rendered["status"])
			assert.NotContains(t, rendered, "url", "判了失败的任务不能把图片地址交给客户")
			assert.NotContains(t, rendered, "data")
		})
	}
}

// 型号 → 上游型号与尺寸：每个对外型号、每个比例发出去的 size，以及不传比例 / auto 的默认。
func TestAsyncImagePlugin_ModelAndRatioMapping(t *testing.T) {
	base := map[string]map[string]string{
		"1K": {"1:1": "1024x1024", "4:3": "1536x1152", "3:2": "1536x1024", "2:3": "1024x1536", "16:9": "1920x1080", "9:16": "1080x1920"},
		"2K": {"1:1": "2048x2048", "4:3": "2048x1536", "3:2": "2560x1712", "2:3": "1712x2560", "16:9": "2048x1152", "9:16": "1152x2048"},
		"4K": {"1:1": "2880x2880", "4:3": "3840x2880", "3:2": "3840x2560", "2:3": "2560x3840", "16:9": "3840x2160", "9:16": "2160x3840"},
	}
	extra := map[string]map[string]string{
		"1K": {"3:4": "1152x1536", "4:5": "1024x1280", "5:4": "1280x1024", "21:9": "2352x1008"},
		"2K": {"3:4": "1728x2304", "4:5": "1792x2240", "5:4": "2240x1792", "21:9": "3024x1296"},
		"4K": {"3:4": "2448x3264", "4:5": "2560x3200", "5:4": "3200x2560", "21:9": "3696x1584"},
	}
	models := []struct {
		name, tier, upstream string
		extended             bool
	}{
		{"gpt-image-2", "1K", "gpt-image-2", false},
		{"gpt-image-2-2K", "2K", "gpt-image-2-pro", false},
		{"gpt-image-2-4K", "4K", "gpt-image-2-pro", false},
		{"gpt-image-2.5-1K", "1K", "gpt-image-2.5-flare", true},
		{"gpt-image-2.5-2K", "2K", "gpt-image-2.5-flare", true},
		{"gpt-image-2.5-4K", "4K", "gpt-image-2.5-flare", true},
	}
	sent := func(modelName, upstreamName string, request map[string]any) map[string]any {
		run, err := asyncImageValidate(t, "https://provider.example", modelName, upstreamName, request)
		require.NoError(t, err, "%s %v", modelName, request)
		body, err := run.adaptor.BuildRequestBody(run.c, run.info)
		require.NoError(t, err)
		url, err := run.adaptor.BuildRequestURL(run.info)
		require.NoError(t, err)
		assert.Equal(t, "https://provider.example"+asyncSubmitPath, url)
		raw, err := io.ReadAll(body)
		require.NoError(t, err)
		got := map[string]any{}
		require.NoError(t, common.Unmarshal(raw, &got))
		return got
	}
	for _, m := range models {
		table := map[string]string{}
		for ratio, size := range base[m.tier] {
			table[ratio] = size
		}
		if m.extended {
			for ratio, size := range extra[m.tier] {
				table[ratio] = size
			}
		}
		for ratio, size := range table {
			got := sent(m.name, m.name, map[string]any{"model": m.name, "prompt": "p", "aspect_ratio": ratio})
			assert.Equal(t, size, got["size"], "%s %s", m.name, ratio)
			assert.Equal(t, m.upstream, got["model"], m.name)
			assert.NotContains(t, got, "aspect_ratio", "上游按像素收，不认比例字段")
			assert.NotContains(t, got, "image", "没有参考图就不带这个字段")
			assert.Equal(t, false, got["watermark"])
			assert.Equal(t, "url", got["response_format"])
			assert.EqualValues(t, 1, got["n"])

			// 客户直接给这一档表里的像素也行。
			got = sent(m.name, m.name, map[string]any{"prompt": "p", "size": strings.ToUpper(size)})
			assert.Equal(t, size, got["size"])
		}
		for _, request := range []map[string]any{
			{"prompt": "p"}, {"prompt": "p", "aspect_ratio": "auto"}, {"prompt": "p", "aspect_ratio": " "},
			{"prompt": "p", "metadata": map[string]any{"aspect_ratio": "1:1"}}, {"prompt": "p", "image_size": strings.ToLower(m.tier)},
		} {
			assert.Equal(t, base[m.tier]["1:1"], sent(m.name, m.name, request)["size"], "%s %v", m.name, request)
		}
	}

	// 渠道配了模型映射：上游型号以渠道为准，档位仍按客户填的对外名。
	got := sent("gpt-image-2.5-4K", "gpt-image-2.5-sunburst", map[string]any{"prompt": "p", "aspect_ratio": "16:9"})
	assert.Equal(t, "gpt-image-2.5-sunburst", got["model"])
	assert.Equal(t, "3840x2160", got["size"])

	// 参考图的四种写法合并去重，按出现顺序。
	got = sent("gpt-image-2.5-1K", "gpt-image-2.5-1K", map[string]any{"prompt": "p",
		"images": []any{"https://r.example/1.png"}, "image": "https://r.example/2.png", "input_reference": "https://r.example/1.png",
		"metadata": map[string]any{"urls": []any{"http://r.example/3.png"}}})
	assert.Equal(t, []any{"https://r.example/1.png", "https://r.example/2.png", "http://r.example/3.png"}, got["image"])
}

// 插件认领 /v1/videos 上这六个型号；并且它不在内置插件里 —— 合并本 PR 不会让任何实例自动启用它。
func TestAsyncImagePlugin_ClaimsVideoProtocolAndIsNotBuiltIn(t *testing.T) {
	registry, plugin := asyncImagePlugin(t)
	assert.Equal(t, "async-image-relay", plugin.Meta.Key)
	assert.Equal(t, "per_task", plugin.Meta.FetchMode)
	assert.Empty(t, plugin.Meta.ChannelTypes, "不认领任何旧渠道类型，只能挂在「任务插件」渠道上")
	assert.Empty(t, plugin.Meta.BaseURL, "上游地址只来自渠道配置")
	assert.Empty(t, plugin.Meta.AllowedHosts)
	assert.Empty(t, plugin.Meta.UsageSchema, "不报用量：按次价由宿主的模型价格决定")
	expected := []string{"gpt-image-2", "gpt-image-2-2K", "gpt-image-2-4K", "gpt-image-2.5-1K", "gpt-image-2.5-2K", "gpt-image-2.5-4K"}
	assert.ElementsMatch(t, expected, plugin.Meta.Models)
	for _, modelName := range expected {
		binding, found := registry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", modelName)
		require.True(t, found, modelName)
		assert.Same(t, plugin, binding.Plugin)
		assert.Equal(t, "openai_video", binding.Protocol)
	}
	for _, hook := range []string{"extractUsage", "extractUsageOnSubmit", "extractUsageOnComplete"} {
		assert.False(t, New(plugin).hasHook(t.Context(), hook), hook)
	}

	_, err := plugins.Source("async-image-relay")
	require.Error(t, err, "不能出现在内置插件目录里")
	_, builtIn := pluginruntime.DefaultRegistry.Get("async-image-relay")
	assert.False(t, builtIn)
	for _, modelName := range expected {
		_, claimed := pluginruntime.DefaultRegistry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", modelName)
		assert.False(t, claimed, "没上传这个插件时，%s 仍走原来的路", modelName)
	}
}

// 源码是公开的：不许写死任何上游地址（地址只能来自渠道的 base_url）。
func TestAsyncImagePlugin_SourceHasNoHardcodedHost(t *testing.T) {
	source := asyncImageSource(t)
	assert.NotContains(t, source, "http://")
	assert.NotContains(t, source, "https://")
	assert.False(t, regexp.MustCompile(`sk-[A-Za-z0-9]{8,}`).MatchString(source), "不许出现密钥样的字符串")
}

// 成品图作为产物暴露（/v1/tasks/:id/artifacts 用）：不带渠道凭据直接取上游给的公开地址；没出图就没有产物。
func TestAsyncImagePlugin_Artifacts(t *testing.T) {
	_, plugin := asyncImagePlugin(t)
	adaptor := New(plugin)
	adaptor.Init(overrideInfo("gpt-image-2", "gpt-image-2", nil))
	done := &model.Task{TaskID: "task_public", Status: model.TaskStatusSuccess, Data: []byte(`{"data":[{"url":"https://cdn.example/a.png"}],"size":"1024x1024"}`)}
	artifacts, err := adaptor.ListArtifacts(done)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	assert.Equal(t, "image", artifacts[0].Key)
	assert.Equal(t, "image", artifacts[0].Type)
	descriptor, err := adaptor.BuildContentRequest(done, "image", channel.TaskArtifactClientRequest{Method: http.MethodGet})
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example/a.png", descriptor.URL)
	assert.True(t, descriptor.Credentialless)
	assert.Empty(t, descriptor.Headers, "取图不带渠道密钥")
	_, err = adaptor.BuildContentRequest(done, "video", channel.TaskArtifactClientRequest{Method: http.MethodGet})
	require.Error(t, err)

	failed := &model.Task{TaskID: "task_public", Status: model.TaskStatusFailure, Data: done.Data}
	artifacts, err = adaptor.ListArtifacts(failed)
	require.NoError(t, err)
	assert.Empty(t, artifacts, "判了失败的任务没有产物")
}
