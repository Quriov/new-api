package jsplugin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// Quriov 改造 6：插件在 buildSubmitRequest 里主动拒单时，客户只看到插件自己写的那句话。
//
// 背景：引擎把 JS 抛出的异常包成 HookError，Error() 是
// 「plugin <key>@<version> hook <name> failed: Error: <message> at <fn> (<file>.js:<行>:<列>)」，
// Message 字段才是插件写的那句话（已截断、去控制字符）。插件路由的解码阶段
// （middleware/task_plugin.go 的 taskPluginHookDetail）早就只回 Message；
// 旧式路由（sora 插件接渠道类型 1）走到这里时却把整个 Error() 回给了客户，露出插件名、版本、钩子名和栈。
//
// 做法：只对 HookError（插件抛出的 JS 异常）把客户可见的 Message 换成 HookError.Message；
// 完整原文留在 TaskError.Error 里，并在这里打一条 warn 日志。超时 / 中断等非 HookError 不动。
const quriovPluginRejectFallback = "请求参数不符合该型号要求"

func quriovPluginRequestInvalid(c *gin.Context, pluginKey string, err error) *dto.TaskError {
	taskErr := service.TaskErrorWrapperLocal(err, "plugin_request_invalid", http.StatusBadRequest)
	var hookErr *pluginruntime.HookError
	if !errors.As(err, &hookErr) {
		return taskErr
	}
	message := strings.TrimSpace(hookErr.Message)
	if message == "" || message == "plugin hook failed" {
		message = quriovPluginRejectFallback
	}
	taskErr.Message = message
	logger.LogWarn(c, "task_plugin subsystem=adaptor event=request_rejected plugin=%q hook=%q err=%q", pluginKey, hookErr.Hook, err.Error())
	return taskErr
}
