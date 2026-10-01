package relay

import (
	"errors"
	"fmt"
	"io"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"

	"github.com/gin-gonic/gin"
)

// ResubmitResult 是一次换渠道重投打到上游之后拿回来的东西。
type ResubmitResult struct {
	UpstreamTaskID string
	TaskData       []byte
	PluginState    []byte
	Platform       constant.TaskPlatform
}

// ResubmitTaskToChannel 把一个【已经提交过】的任务，用原始请求重新打到另一个渠道上。
//
// 跟 RelayTaskSubmit 的区别只有一处、但很关键：**完全不碰计费**。
// 额度在首次提交时就已经预扣了，这次重投沿用同一笔——
// 任务最终成功就结算、最终失败才退款，客户不会因为我们换腿而被收两次钱。
// 也因此这里不算价、不预扣、不改 PriceData。
//
// 其余步骤与 RelayTaskSubmit 保持同序（上游 rc.27 起任务适配器换成了 JS 插件）：
// 刷新渠道元数据 → 按新渠道类型解析插件 → 先做模型映射 → 校验并构建提交 →
// 发送 → 解析。调用方负责准备好 c（带原始请求体）和 info（带新渠道的 meta）。
func ResubmitTaskToChannel(c *gin.Context, info *relaycommon.RelayInfo) (*ResubmitResult, error) {
	info.InitChannelMeta(c)

	platform := constant.TaskPlatform(c.GetString("platform"))
	if platform == "" {
		platform = GetTaskPlatform(c)
	}
	platform, adaptor := getTaskAdaptorForRequest(c, platform)
	if adaptor == nil {
		_, message := TaskPlatformUnavailableError(platform)
		return nil, fmt.Errorf("新渠道没有可用的任务插件: %s", message)
	}
	adaptor.Init(info)

	// 新渠道可能有自己的模型映射，必须重算一遍，不能沿用旧渠道的。
	// 跟 RelayTaskSubmit 一样放在校验之前：插件的 buildSubmitRequest 在校验阶段就会跑，
	// 它读的是映射后的 upstreamModel。
	info.UpstreamModelName = info.OriginModelName
	if err := helper.ModelMappedHelper(c, info, nil); err != nil {
		return nil, fmt.Errorf("重投时模型映射失败: %w", err)
	}

	if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
		return nil, fmt.Errorf("重投时请求校验失败: %s", taskErr.Message)
	}

	requestBody, err := adaptor.BuildRequestBody(c, info)
	if err != nil {
		return nil, fmt.Errorf("重投时构建请求体失败: %w", err)
	}

	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return nil, fmt.Errorf("重投时发送请求失败: %w", err)
	}
	if resp == nil {
		return nil, errors.New("重投时上游没有返回响应")
	}
	defer resp.Body.Close()
	// 跟 RelayTaskSubmit 一致：任何 2xx 都算提交成功（201/202 也是）。
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("重投被上游拒绝，HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	parsed, taskErr := adaptor.ParseResponse(c, resp, info)
	if taskErr != nil {
		return nil, fmt.Errorf("重投时解析上游响应失败: %s", taskErr.Message)
	}
	if parsed == nil {
		return nil, errors.New("重投时插件返回了空结果")
	}
	// 提交即终态（同步出结果）的插件不走这条路：重投的后续处理只认「拿到新任务 ID、继续轮询」。
	// 真遇到了按失败处理，任务照原来的失败流程走（终态 + 退款），不会误收钱。
	if parsed.Immediate != nil && (parsed.Immediate.Status == model.TaskStatusSuccess || parsed.Immediate.Status == model.TaskStatusFailure) {
		return nil, fmt.Errorf("新渠道的插件在提交时就返回了终态(%s)，重投暂不支持这种插件", parsed.Immediate.Status)
	}
	if parsed.UpstreamTaskID == "" {
		return nil, errors.New("重投后上游没有返回任务 ID，无法继续跟踪")
	}
	return &ResubmitResult{
		UpstreamTaskID: parsed.UpstreamTaskID,
		TaskData:       parsed.TaskData,
		PluginState:    parsed.PluginState,
		Platform:       platform,
	}, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
