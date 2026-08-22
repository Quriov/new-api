package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

// 任务失败后换渠道重投。
//
// 要解决的问题：new-api 的渠道重试只覆盖【提交阶段】——上游一旦收下任务、
// 任务落库返回给客户，之后上游把它跑失败了，就是终态，不会去试别的渠道。
// 实测(2026-08-21)：某天 28 次任务失败全部落在同一个渠道上，
// 旁边配好并且启用着的备用渠道一次都没有被试过。
//
// 这里做的事情很窄：任务在轮询阶段被判定失败时，如果这个失败看起来是
// 「换一家上游有可能成功」的那一类，就挑一个还没试过的渠道把【原始请求】
// 重发一次，然后让这个任务继续走轮询，而不是就地终结 + 退款。
//
// 刻意不做的几件事：
//   - 不新建任务行：客户拿到的 task_id 不变，他不需要知道我们换了腿。
//   - 不重新计费：额度在首次提交时已经预扣，重投沿用同一笔，
//     最终成功就结算、最终失败才退款。所以重投不会产生第二次收费。
//   - 不碰任何定价/渠道优先级策略：那些是我们自己的东西，不进这个仓。

// ResubmitTaskFunc 由 main 包注入，执行真正的「拿原始请求打新渠道」。
// 走注入而不是直接调用，是为了不破坏 service -> relay 的依赖方向
// （跟隔壁 GetTaskAdaptorFunc 同一个套路）。
var ResubmitTaskFunc func(ctx context.Context, task *model.Task, ch *model.Channel) (upstreamTaskID string, taskData []byte, platform constant.TaskPlatform, err error)

// ResubmitDecision 说明「这次失败要不要换渠道重投」以及为什么。
// Reason 会进日志，写成人话，方便事后一眼看懂当时为什么没重投。
type ResubmitDecision struct {
	ShouldResubmit bool
	Reason         string
}

// DecideResubmit 判断一次任务失败要不要换渠道重投。纯函数，不碰数据库也不发请求。
func DecideResubmit(task *model.Task, failReason string) ResubmitDecision {
	if !constant.TaskResubmitEnabled {
		return ResubmitDecision{false, "换渠道重投未开启"}
	}
	if task == nil {
		return ResubmitDecision{false, "任务为空"}
	}
	if constant.TaskResubmitMaxAttempts <= 0 {
		return ResubmitDecision{false, "重投次数上限配置为 0"}
	}
	if task.PrivateData.ResubmitCount >= constant.TaskResubmitMaxAttempts {
		return ResubmitDecision{false, fmt.Sprintf("已重投 %d 次，达到上限 %d",
			task.PrivateData.ResubmitCount, constant.TaskResubmitMaxAttempts)}
	}
	if _, ok := task.ResubmitPayload(); !ok {
		return ResubmitDecision{false, "没有留存原始请求（可能是请求体超过留存上限，或任务在本功能上线前创建）"}
	}
	if reason, hit := matchSkipReason(failReason); hit {
		return ResubmitDecision{false, fmt.Sprintf("失败原因命中「换家上游也没用」清单：%s", reason)}
	}
	return ResubmitDecision{true, "上游侧失败，尝试换一个渠道重投"}
}

// matchSkipReason 判断失败原因是不是「换家上游也一样会失败」的那一类。
// 典型是内容审核 / 提示词被拒——重投只会把一次收费变两次，还多等一轮。
// 大小写不敏感（上游的英文报错大小写不统一）。
func matchSkipReason(failReason string) (string, bool) {
	if strings.TrimSpace(failReason) == "" {
		// 没给原因：当作上游侧问题，允许重投。
		return "", false
	}
	lowered := strings.ToLower(failReason)
	for _, pattern := range constant.TaskResubmitSkipReasons {
		p := strings.ToLower(strings.TrimSpace(pattern))
		if p == "" {
			continue
		}
		if strings.Contains(lowered, p) {
			return pattern, true
		}
	}
	return "", false
}

// PickResubmitChannel 从同分组同模型里挑一个还没试过的渠道。
// 挑不到就返回 nil（这不是错误，是「确实没有第二条腿」）。
func PickResubmitChannel(group, modelName, requestPath string, tried []int) (*model.Channel, error) {
	if modelName == "" {
		return nil, fmt.Errorf("模型名为空，无法挑渠道")
	}
	triedSet := make(map[int]bool, len(tried))
	for _, id := range tried {
		triedSet[id] = true
	}
	// GetRandomSatisfiedChannel 的 retry 参数走的是【优先级档位】：
	// 0 = 最高优先级，往上递增就是逐级降档。这里把所有档位扫一遍，
	// 跳过已经试过的，第一个没试过的就是答案。
	// 上限用一个够大的常数：函数内部会把超出的 retry 夹到最后一档，
	// 所以扫到重复即可停。
	const maxPriorityTiers = 16
	var lastPicked int
	for tier := 0; tier < maxPriorityTiers; tier++ {
		ch, err := model.GetRandomSatisfiedChannel(group, modelName, tier, requestPath)
		if err != nil {
			return nil, err
		}
		if ch == nil {
			break
		}
		if !triedSet[ch.Id] {
			return ch, nil
		}
		if ch.Id == lastPicked && tier > 0 {
			// 已经夹到最后一档、还在返回同一个渠道，再扫下去也是它。
			break
		}
		lastPicked = ch.Id
	}
	return nil, nil
}

// TryResubmitTaskOnAnotherChannel 是轮询侧的唯一入口。
//
// 返回 true 表示【任务已经被重投到另一个渠道】，调用方必须据此改变行为：
// 不要把任务置成终态失败，也不要退款——它还活着，下一轮轮询会继续跟。
// 返回 false 表示没重投（原因已写进日志），调用方照原来的失败流程走。
//
// 这个函数只在成功时修改 task 的内存字段（渠道、上游任务 ID、状态等），
// 落库由调用方跟它自己那次 CAS 更新一起做，避免两处各写一次。
func TryResubmitTaskOnAnotherChannel(ctx context.Context, task *model.Task, failReason string) bool {
	decision := DecideResubmit(task, failReason)
	if !decision.ShouldResubmit {
		logger.LogDebug(ctx, fmt.Sprintf("任务 %s 不重投：%s", task.TaskID, decision.Reason))
		return false
	}
	if ResubmitTaskFunc == nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 想重投但重投器没有接线（ResubmitTaskFunc 为空）", task.TaskID))
		return false
	}

	tried := task.TriedChannelIDs()
	modelName := task.Properties.OriginModelName
	if modelName == "" {
		modelName = task.Properties.UpstreamModelName
	}
	ch, err := PickResubmitChannel(task.Group, modelName, task.PrivateData.ResubmitPath, tried)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 挑重投渠道出错：%s", task.TaskID, err.Error()))
		return false
	}
	if ch == nil {
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s 想换渠道重投，但模型 %s 在分组 %s 下没有别的渠道可换（已试过 %v）",
			task.TaskID, modelName, task.Group, tried))
		return false
	}

	upstreamTaskID, taskData, platform, err := ResubmitTaskFunc(ctx, task, ch)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 重投到渠道 #%d(%s) 失败：%s",
			task.TaskID, ch.Id, ch.Name, err.Error()))
		// 这一次重投本身失败了，也要把这个渠道记进「试过」，
		// 否则下一轮轮询会一直挑中同一个坏渠道。
		task.MarkChannelTried(ch.Id)
		return false
	}

	fromChannel := task.ChannelId
	task.MarkChannelTried(ch.Id)
	task.ChannelId = ch.Id
	task.PrivateData.UpstreamTaskID = upstreamTaskID
	task.PrivateData.ResubmitCount++
	task.PrivateData.Key = "" // 换渠道了，旧渠道的 key 不能再带着
	if platform != "" && platform != task.Platform {
		// 新渠道的类型可能跟老的不一样。轮询是按 platform 分组挑适配器的，
		// 这里不跟着改，下一轮就会拿错适配器去查这个任务。
		task.Platform = platform
	}
	if len(taskData) > 0 {
		task.Data = taskData
	}
	// 回到「在跑」状态，让下一轮轮询继续跟它。
	task.Status = model.TaskStatusInProgress
	task.Progress = "0%"
	task.FailReason = ""
	task.FinishTime = 0

	logger.LogInfo(ctx, fmt.Sprintf("任务 %s 上游失败(%s)，已从渠道 #%d 换到 #%d(%s) 重投，第 %d 次；上游新任务 ID %s",
		task.TaskID, truncateForLog(failReason, 80), fromChannel, ch.Id, ch.Name,
		task.PrivateData.ResubmitCount, upstreamTaskID))
	return true
}

func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
