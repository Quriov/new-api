package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

// ResubmitTaskOnChannel 是「任务失败换渠道重投」的执行端。
//
// 轮询跑在后台协程里，手上没有 gin.Context，而整条提交链路（适配器、模型映射、
// 渠道 meta）都是围绕 gin.Context 写的。所以这里用留存的原始请求体造一个
// 等价的上下文出来，走跟真实请求完全相同的那条路——而不是另写一套发请求的代码。
// 另写一套的代价是它会跟真实路径慢慢分叉，而分叉的地方只有出事那天才会被发现。
func ResubmitTaskOnChannel(ctx context.Context, task *model.Task, ch *model.Channel) (string, []byte, constant.TaskPlatform, error) {
	if task == nil || ch == nil {
		return "", nil, "", fmt.Errorf("任务或渠道为空")
	}
	body, ok := task.ResubmitPayload()
	if !ok {
		return "", nil, "", fmt.Errorf("没有留存原始请求，无法重投")
	}

	modelName := task.Properties.OriginModelName
	if modelName == "" {
		modelName = task.Properties.UpstreamModelName
	}
	if modelName == "" {
		return "", nil, "", fmt.Errorf("任务上没有记录模型名，无法重投")
	}

	path := task.PrivateData.ResubmitPath
	if path == "" {
		return "", nil, "", fmt.Errorf("任务上没有记录请求路径，无法重投")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return "", nil, "", fmt.Errorf("构造重投请求失败: %w", err)
	}
	if ct := task.PrivateData.ResubmitContentType; ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.ContentLength = int64(len(body))

	c, _ := gin.CreateTestContext(nullWriter{})
	c.Request = req

	// 把提交链路会读的那几个上下文键补上。少了它们不会报错，
	// 只会安静地拿到零值（用户 ID 变 0、分组变空），所以必须显式写。
	common.SetContextKey(c, constant.ContextKeyUserId, task.UserId)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, task.Group)
	common.SetContextKey(c, constant.ContextKeyUserGroup, task.Group)
	common.SetContextKey(c, constant.ContextKeyTokenId, task.PrivateData.TokenId)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, modelName)

	// 渠道 meta 走跟真实请求一模一样的那个函数，避免两边对「渠道怎么装进上下文」
	// 的理解产生分叉。它同时会设置 channel_type，下游据此选适配器。
	if setupErr := middleware.SetupContextForSelectedChannel(c, ch, modelName); setupErr != nil {
		return "", nil, "", fmt.Errorf("装载渠道 #%d 失败: %s", ch.Id, setupErr.Error())
	}

	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		return "", nil, "", fmt.Errorf("构造 relay info 失败: %w", err)
	}
	info.Action = task.Action
	info.OriginModelName = modelName
	// ChannelMeta (which owns UpstreamModelName) is initialized inside
	// ResubmitTaskToChannel. Touching the promoted field before that call
	// dereferences a nil ChannelMeta and crashes the polling goroutine.
	if info.TaskRelayInfo != nil {
		// 沿用原来的公开任务 ID：客户手上的 task_id 不变，
		// 他不需要知道我们在底下换了一条腿。
		info.PublicTaskID = task.TaskID
	}

	upstreamTaskID, taskData, err := relay.ResubmitTaskToChannel(c, info)
	if err != nil {
		return "", nil, "", err
	}
	return upstreamTaskID, taskData, relay.GetTaskPlatform(c), nil
}

// nullWriter 满足 gin.CreateTestContext 对 http.ResponseWriter 的要求。
// 重投没有任何东西要写回客户端——客户早在首次提交时就拿到响应了。
type nullWriter struct{ h http.Header }

func (w nullWriter) Header() http.Header {
	if w.h == nil {
		return http.Header{}
	}
	return w.h
}
func (w nullWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w nullWriter) WriteHeader(int)             {}

// channelIDForResubmit 取本次实际用上的渠道 ID（重试过的话就是最后那个）。
func channelIDForResubmit(c *gin.Context) int {
	return common.GetContextKeyInt(c, constant.ContextKeyChannelId)
}

// recordResubmitPayload 把原始请求存进任务，供后续换渠道重投。
// 存不下就算了——宁可放弃这个任务的重投能力，也不要把几 MB 的图片塞进 tasks 表。
func recordResubmitPayload(c *gin.Context, task *model.Task) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return
	}
	body, err := storage.Bytes()
	if err != nil || len(body) == 0 {
		return
	}
	limit := constant.TaskResubmitMaxBodyKB * 1024
	if !task.SetResubmitPayload(body, c.GetHeader("Content-Type"), c.Request.URL.Path, limit) && len(body) > limit {
		common.SysLog(fmt.Sprintf("任务 %s 的请求体 %d 字节超过留存上限 %d 字节，该任务失败后将无法换渠道重投",
			task.TaskID, len(body), limit))
	}
}

// SyncImageRelayOnChannel 对【只有同步接口】的上游直接出图。
//
// 跟 ResubmitTaskOnChannel 的区别：那个是"再提交一个异步任务、之后继续轮询"，
// 这个是"当场把图要出来"。所以它返回的是图片地址，不是任务 ID。
//
// ⚠ 会阻塞几十秒。只在轮询协程里调 —— 客户请求链路前面有网关超时，
//
//	当初把出图切成异步就是为了躲那个上限。
func SyncImageRelayOnChannel(ctx context.Context, task *model.Task, ch *model.Channel) (string, []byte, error) {
	if task == nil || ch == nil {
		return "", nil, fmt.Errorf("任务或渠道为空")
	}
	body, ok := task.ResubmitPayload()
	if !ok {
		return "", nil, fmt.Errorf("没有留存原始请求，无法走同步腿")
	}
	upstreamModel := task.Properties.UpstreamModelName
	if upstreamModel == "" {
		upstreamModel = task.Properties.OriginModelName
	}
	if upstreamModel == "" {
		return "", nil, fmt.Errorf("任务上没有记录模型名，无法走同步腿")
	}
	//: 新渠道可能有自己的模型映射（比如上游那边这个模型叫别的名字）。
	//: GetModelMapping 返回的是一段 JSON 字符串，不是 map。
	if raw := ch.GetModelMapping(); raw != "" {
		mapping := make(map[string]string)
		if err := common.Unmarshal([]byte(raw), &mapping); err == nil {
			if v, hit := mapping[upstreamModel]; hit && v != "" {
				upstreamModel = v
			}
		}
	}

	result, err := relay.CallSyncImageRelay(ctx, ch, body, upstreamModel)
	if err != nil {
		return "", nil, err
	}
	return result.ImageURL, result.RawBody, nil
}
