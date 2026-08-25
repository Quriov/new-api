package service

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// SyncImageRelayFunc 由 main 包注入，对【只有同步接口】的上游直接出图。
// 同样走注入，理由跟上面一样：不破坏 service -> relay 的依赖方向。
//
// 返回的是【已经出好的图】的地址，不是一个待轮询的任务 ID ——
// 所以调用方拿到它之后要把任务直接置成成功，而不是继续轮询。
var SyncImageRelayFunc func(ctx context.Context, task *model.Task, ch *model.Channel) (imageURL string, rawBody []byte, err error)

// ResubmitOutcome 是一次重投尝试的结果。三态而不是布尔，
// 因为「换了腿继续轮询」和「换的那条腿是同步的、图已经出来了」对调用方
// 是两种完全不同的后续处理：前者不能结算也不能退款，后者要结算。
type ResubmitOutcome int

const (
	// ResubmitNone 没有重投，调用方照原来的失败流程走（终态 + 退款）。
	ResubmitNone ResubmitOutcome = iota
	// ResubmitAsync 已重投到另一个异步渠道，任务还活着 —— 不要终结、不要退款。
	ResubmitAsync
	// ResubmitCompleted 换到的是同步渠道，图已经出来了 —— 任务直接成功，要结算。
	ResubmitCompleted
)

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

	// 异步渠道都试过了，才轮到「只有同步接口」的那条腿。
	//
	// ⚠ 顺序是刻意的：同步腿要阻塞几十秒，而异步渠道提交完就返回。
	//   先试便宜的，实在没得换了再上贵的。
	//   而且同步腿往往是唯一一家【不同源】的上游 —— 留到最后，正好覆盖
	//   「主通道整家塌掉」这种最坏情况。
	for _, ch := range model.GetSyncRelayChannels(group, modelName) {
		if !triedSet[ch.Id] {
			return ch, nil
		}
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
func TryResubmitTaskOnAnotherChannel(ctx context.Context, task *model.Task, failReason string) ResubmitOutcome {
	decision := DecideResubmit(task, failReason)
	if !decision.ShouldResubmit {
		logger.LogDebug(ctx, fmt.Sprintf("任务 %s 不重投：%s", task.TaskID, decision.Reason))
		return ResubmitNone
	}
	tried := task.TriedChannelIDs()
	modelName := task.Properties.OriginModelName
	if modelName == "" {
		modelName = task.Properties.UpstreamModelName
	}
	ch, err := PickResubmitChannel(task.Group, modelName, task.PrivateData.ResubmitPath, tried)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 挑重投渠道出错：%s", task.TaskID, err.Error()))
		return ResubmitNone
	}
	if ch == nil {
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s 想换渠道重投，但模型 %s 在分组 %s 下没有别的渠道可换（已试过 %v）",
			task.TaskID, modelName, task.Group, tried))
		return ResubmitNone
	}

	// 换到的是「只有同步接口」的那条腿 —— 直接把图出出来，任务当场结束。
	if ch.GetSetting().QuriovSyncImageRelay {
		return resubmitViaSyncRelay(ctx, task, ch, failReason)
	}

	// ⚠ 这个检查必须放在这里, 不能放在函数开头 —— 同步腿那条路压根不用它。
	//   放在开头的话, 只配了同步腿的部署会在这里被挡回去, 而且失败方式是静默的
	//   (日志里说"没接线", 看起来像配置问题, 实际是判据放错了地方)。
	if ResubmitTaskFunc == nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 想重投但异步重投器没有接线（ResubmitTaskFunc 为空）", task.TaskID))
		return ResubmitNone
	}

	upstreamTaskID, taskData, platform, err := ResubmitTaskFunc(ctx, task, ch)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 重投到渠道 #%d(%s) 失败：%s",
			task.TaskID, ch.Id, ch.Name, err.Error()))
		// 这一次重投本身失败了，也要把这个渠道记进「试过」，
		// 否则下一轮轮询会一直挑中同一个坏渠道。
		task.MarkChannelTried(ch.Id)
		return ResubmitNone
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
	return ResubmitAsync
}

// resubmitViaSyncRelay 换到只有同步接口的上游：直接把图出出来，任务当场结束。
//
// ⚠ 这里会阻塞几十秒。可以这么做的前提是：本函数只被轮询协程调用，
//
//	前面没有客户的 HTTP 连接、也没有网关超时。渠道之间是并行轮询的，
//	所以这段阻塞只会拖慢同一个渠道上排在后面的任务，不影响别的渠道。
func resubmitViaSyncRelay(ctx context.Context, task *model.Task, ch *model.Channel, failReason string) ResubmitOutcome {
	if SyncImageRelayFunc == nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 想走同步腿但同步出图器没有接线（SyncImageRelayFunc 为空）", task.TaskID))
		return ResubmitNone
	}
	fromChannel := task.ChannelId
	start := time.Now()

	imageURL, rawBody, err := SyncImageRelayFunc(ctx, task, ch)
	// 无论成败都记「试过」——失败时下一轮不该再挑中它。
	task.MarkChannelTried(ch.Id)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("任务 %s 走同步腿渠道 #%d(%s) 失败（耗时 %.0fs）：%s",
			task.TaskID, ch.Id, ch.Name, time.Since(start).Seconds(), err.Error()))
		return ResubmitNone
	}

	task.ChannelId = ch.Id
	task.PrivateData.ResubmitCount++
	task.PrivateData.Key = ""
	task.PrivateData.ResultURL = imageURL
	task.Status = model.TaskStatusSuccess
	task.Progress = "100%"
	task.FailReason = ""
	task.FinishTime = time.Now().Unix()
	if len(rawBody) > 0 {
		task.Data = rawBody
	}

	logger.LogInfo(ctx, fmt.Sprintf("任务 %s 上游失败(%s)，已从渠道 #%d 换到同步腿 #%d(%s) 并直接出图成功，耗时 %.0fs",
		task.TaskID, truncateForLog(failReason, 80), fromChannel, ch.Id, ch.Name, time.Since(start).Seconds()))
	return ResubmitCompleted
}

func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
