package relay

import (
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"

	"github.com/gin-gonic/gin"
)

// ResubmitTaskToChannel 把一个【已经提交过】的任务，用原始请求重新打到另一个渠道上。
//
// 跟 RelayTaskSubmit 的区别只有一处、但很关键：**完全不碰计费**。
// 额度在首次提交时就已经预扣了，这次重投沿用同一笔——
// 任务最终成功就结算、最终失败才退款，客户不会因为我们换腿而被收两次钱。
// 也因此这里不算价、不预扣、不改 PriceData。
//
// 调用方负责准备好 c（带原始请求体）和 info（带新渠道的 meta）。
func ResubmitTaskToChannel(c *gin.Context, info *relaycommon.RelayInfo) (string, []byte, error) {
	info.InitChannelMeta(c)

	platform := constant.TaskPlatform(c.GetString("platform"))
	if platform == "" {
		platform = GetTaskPlatform(c)
	}
	adaptor := GetTaskAdaptor(platform)
	if adaptor == nil {
		return "", nil, fmt.Errorf("找不到平台 %s 的任务适配器", platform)
	}
	adaptor.Init(info)

	if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
		return "", nil, fmt.Errorf("重投时请求校验失败: %s", taskErr.Message)
	}

	// 新渠道可能有自己的模型映射，必须重算一遍，不能沿用旧渠道的。
	if err := helper.ModelMappedHelper(c, info, nil); err != nil {
		return "", nil, fmt.Errorf("重投时模型映射失败: %w", err)
	}

	requestBody, err := adaptor.BuildRequestBody(c, info)
	if err != nil {
		return "", nil, fmt.Errorf("重投时构建请求体失败: %w", err)
	}

	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return "", nil, fmt.Errorf("重投时发送请求失败: %w", err)
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return "", nil, fmt.Errorf("重投被上游拒绝，HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	upstreamTaskID, taskData, taskErr := adaptor.DoResponse(c, resp, info)
	if taskErr != nil {
		return "", nil, fmt.Errorf("重投时解析上游响应失败: %s", taskErr.Message)
	}
	if upstreamTaskID == "" {
		return "", nil, fmt.Errorf("重投后上游没有返回任务 ID，无法继续跟踪")
	}
	return upstreamTaskID, taskData, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
