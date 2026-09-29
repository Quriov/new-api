package model

import (
	"slices"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
)

var filterEvalOrder = []dto.ChannelFilterKind{
	dto.FilterRequestPath,
	dto.FilterTaskPluginIdentity,
	dto.FilterResponsesWebSocket,
}

// ChannelSatisfiesFilters reports whether ch passes every filter.
// On false, it returns the kind of the first violated filter (request_path
// then task_plugin_identity) for error attribution.
func ChannelSatisfiesFilters(ch *Channel, modelName string, filters []dto.ChannelFilter) (bool, dto.ChannelFilterKind) {
	if ch == nil {
		return false, ""
	}
	// Quriov 改造: 只有同步出图接口的渠道对【正常路由】不可见, 无论带不带过滤器。
	// 客户走的是异步任务接口, 把请求发给它必然失败 —— 让它进正常路由等于主动制造故障。
	// 它只在「任务失败后换渠道重投」那条路上被 GetSyncRelayChannels 单独挑出来用。
	// ⚠ 数据库路径(内存缓存关着, 生产就是关着的)走这里; 内存缓存路径走 filterCandidateIDs,
	//   两处都要有 —— 2026-08-25 只改一边、单测全绿、生产照样选中同步腿。
	if isSyncRelayOnly(ch) {
		return false, ""
	}
	for _, kind := range filterEvalOrder {
		for _, filter := range filters {
			if filter.Kind != kind {
				continue
			}
			if !channelMatchesFilter(ch, modelName, filter) {
				return false, kind
			}
		}
	}
	return true, ""
}

// filterCandidateIDs applies filters to a cached candidate id list.
// Caller must hold channelSyncLock (read lock). The input slice is never mutated.
// A missing id in channelsIDM is kept for request_path (downstream consistency
// error) and dropped for task_plugin_identity, matching the previous filters.
func filterCandidateIDs(ids []int, modelName string, filters []dto.ChannelFilter) (kept []int, emptiedBy dto.ChannelFilterKind) {
	if len(ids) == 0 {
		return ids, ""
	}
	kept = withoutSyncRelayOnly(ids)
	for _, kind := range filterEvalOrder {
		kindFilters := filtersByKind(filters, kind)
		if len(kindFilters) == 0 {
			continue
		}
		next := make([]int, 0, len(kept))
		for _, id := range kept {
			channel, exists := channelsIDM[id]
			if candidatePassesKindFilters(channel, exists, modelName, kind, kindFilters) {
				next = append(next, id)
			}
		}
		if len(kept) > 0 && len(next) == 0 {
			return next, kind
		}
		kept = next
	}
	return kept, ""
}

// isSyncRelayOnly 判断渠道是否被标记为「只有同步出图接口」（Quriov 改造，见 QURIOV.md 改造 2）。
func isSyncRelayOnly(ch *Channel) bool {
	return ch != nil && ch.GetSetting().QuriovSyncImageRelay
}

// withoutSyncRelayOnly 从内存缓存的候选 ID 里去掉同步腿（Quriov 改造）。
// 不在 channelsIDM 里的 ID 原样保留（跟上游对缺失 ID 的处理一致，由下游报一致性错误）。
// 调用方必须持有 channelSyncLock 读锁。输入切片不被修改。
func withoutSyncRelayOnly(ids []int) []int {
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if ch, ok := channelsIDM[id]; ok && isSyncRelayOnly(ch) {
			continue
		}
		out = append(out, id)
	}
	return out
}

func filtersByKind(filters []dto.ChannelFilter, kind dto.ChannelFilterKind) []dto.ChannelFilter {
	var matched []dto.ChannelFilter
	for _, filter := range filters {
		if filter.Kind == kind {
			matched = append(matched, filter)
		}
	}
	return matched
}

func candidatePassesKindFilters(ch *Channel, exists bool, modelName string, kind dto.ChannelFilterKind, filters []dto.ChannelFilter) bool {
	if kind == dto.FilterRequestPath && !exists {
		return true
	}
	if !exists || ch == nil {
		return false
	}
	for _, filter := range filters {
		if !channelMatchesFilter(ch, modelName, filter) {
			return false
		}
	}
	return true
}

func channelMatchesFilter(ch *Channel, modelName string, filter dto.ChannelFilter) bool {
	switch filter.Kind {
	case dto.FilterRequestPath:
		if filter.RequestPath == "" {
			return true
		}
		if !constant.IsAdvancedCustomChannel(ch.Type) {
			return true
		}
		config := ch.GetOtherSettings().AdvancedCustom
		return config != nil && config.SupportsPathForModel(filter.RequestPath, modelName)
	case dto.FilterTaskPluginIdentity:
		if filter.TaskPluginKey == "" {
			return ch.Type != constant.ChannelTypeTaskPlugin
		}
		if ch.Type == constant.ChannelTypeTaskPlugin || ch.Type == constant.ChannelTypeNewAPI {
			// A New API channel serves every plugin it is extended with; the
			// pinned plugin or any shared-model candidate may execute there.
			setting := ch.GetSetting()
			return setting.BindsTaskPlugin(filter.TaskPluginKey) || slices.ContainsFunc(filter.TaskPluginKeys, setting.BindsTaskPlugin)
		}
		return slices.Contains(filter.TaskPluginChannelTypes, ch.Type)
	case dto.FilterResponsesWebSocket:
		if !ch.GetSetting().ResponsesWebSocketEnabled {
			return false
		}
		switch ch.Type {
		case constant.ChannelTypeOpenAI, constant.ChannelTypeCodex, constant.ChannelTypeSub2API, constant.ChannelTypeNewAPI:
			return true
		case constant.ChannelTypeAdvancedCustom:
			// The session forwards native Responses events without protocol
			// conversion, so only a converter-free /v1/responses route qualifies.
			route, ok := ch.GetOtherSettings().AdvancedCustom.MatchPathForModel("/v1/responses", modelName)
			return ok && route.IsNative()
		default:
			return false
		}
	default:
		return true
	}
}
