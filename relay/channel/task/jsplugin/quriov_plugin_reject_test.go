package jsplugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Quriov 改造 6：buildSubmitRequest 里插件抛错时，客户回应只剩插件写的那句话；
// 插件 key / 版本 / 钩子名 / 栈只留在内部 error（和日志）里。真插件引擎跑，不 mock 引擎。
func TestPluginRejectShowsOnlyPluginMessage(t *testing.T) {
	cases := []struct {
		name  string
		throw string
		want  string
	}{
		{name: "Error 对象", throw: `throw new Error("xyz");`, want: "xyz"},
		{name: "首尾空白去掉", throw: `throw new Error("  xyz  ");`, want: "xyz"},
		{name: "空 message 退回通用话", throw: `throw new Error("");`, want: quriovPluginRejectFallback},
		{name: "插件 bug(TypeError)只给异常文本、不带前缀和栈", throw: `var o; o.x.y = 1;`, want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := strings.Replace(mockPlugin,
				`if (!ctx.requestBody.prompt) throw new Error("prompt required");`,
				testCase.throw, 1)
			require.NotEqual(t, mockPlugin, source)
			plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
			require.NoError(t, err)

			gin.SetMode(gin.TestMode)
			adaptor := New(plugin)
			info := &relaycommon.RelayInfo{
				ChannelMeta:     &relaycommon.ChannelMeta{ChannelBaseUrl: "https://provider.example"},
				TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
				OriginModelName: "mock-v1",
			}
			adaptor.Init(info)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
			c.Set("task_request", map[string]any{"model": "mock-v1", "prompt": "p"})

			taskErr := adaptor.ValidateRequestAndSetAction(c, info)
			require.NotNil(t, taskErr)
			assert.Equal(t, "plugin_request_invalid", taskErr.Code)
			assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)

			// 客户拿到的就是 c.JSON(status, taskErr) 的这段 JSON。
			raw, err := common.Marshal(taskErr)
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, common.Unmarshal(raw, &body))
			message, _ := body["message"].(string)
			if testCase.want != "" {
				assert.Equal(t, testCase.want, message)
			} else {
				assert.NotEmpty(t, message)
			}
			for _, leak := range []string{"plugin ", "hook ", ".js:", "mock-task", "1.0.0", "buildSubmitRequest", " at "} {
				assert.NotContains(t, string(raw), leak, "客户回应不该露出 %q", leak)
			}

			// 内部 error 保留完整原文（日志用它）。
			require.Error(t, taskErr.Error)
			full := taskErr.Error.Error()
			assert.Contains(t, full, "plugin mock-task@1.0.0 hook buildSubmitRequest failed")
		})
	}
}
