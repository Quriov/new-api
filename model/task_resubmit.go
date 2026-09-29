package model

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// 任务失败后换渠道重投（Quriov 改造）在 Task 上需要的读写辅助。
//
// 存在的理由：new-api 原本只在【提交阶段】做渠道重试——一旦上游收下了任务、
// 任务落了库，之后无论上游怎么失败都不会再换渠道。实测某天 28 次任务失败
// 全部落在同一个渠道上，旁边配好的备用渠道一次都没被试过。
//
// ⚠ 这些字段一律存在 TaskPrivateData 里（不返回给用户）。
//   TaskPrivateData.Value() 是逐字段判空的（上游 rc.28 起），加字段要同步改那里。

// TriedChannelIDs 返回这个任务已经试过的渠道 ID（含首次提交那个）。
func (t *Task) TriedChannelIDs() []int {
	raw := strings.TrimSpace(t.PrivateData.TriedChannels)
	if raw == "" {
		// 老任务没有这个字段：至少当前渠道是试过的。
		if t.ChannelId != 0 {
			return []int{t.ChannelId}
		}
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, p := range parts {
		id, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if t.ChannelId != 0 && !seen[t.ChannelId] {
		out = append(out, t.ChannelId)
	}
	return out
}

// MarkChannelTried 把一个渠道记进「试过」清单（幂等）。
func (t *Task) MarkChannelTried(channelID int) {
	if channelID == 0 {
		return
	}
	for _, id := range t.TriedChannelIDs() {
		if id == channelID {
			// 已在清单里，但老任务可能还没落过字段，补一次。
			t.PrivateData.TriedChannels = joinIDs(t.TriedChannelIDs())
			return
		}
	}
	t.PrivateData.TriedChannels = joinIDs(append(t.TriedChannelIDs(), channelID))
}

func joinIDs(ids []int) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.Itoa(id))
	}
	return strings.Join(parts, ",")
}

// SetResubmitPayload 留存原始请求，供后续换渠道重投使用。
// bodyLimitBytes <= 0 表示不留存。超过上限也不留存——宁可放弃重投，
// 也不要把几 MB 的图片塞进 tasks 表里。返回是否真的存下来了。
func (t *Task) SetResubmitPayload(body []byte, contentType, path string, bodyLimitBytes int) bool {
	if bodyLimitBytes <= 0 || len(body) == 0 || len(body) > bodyLimitBytes {
		return false
	}
	t.PrivateData.ResubmitBodyB64 = base64.StdEncoding.EncodeToString(body)
	t.PrivateData.ResubmitContentType = contentType
	t.PrivateData.ResubmitPath = path
	return true
}

// ResubmitPayload 取回原始请求体。第二个返回值表示这个任务能不能重投。
func (t *Task) ResubmitPayload() ([]byte, bool) {
	if t.PrivateData.ResubmitBodyB64 == "" {
		return nil, false
	}
	body, err := base64.StdEncoding.DecodeString(t.PrivateData.ResubmitBodyB64)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	return body, true
}
