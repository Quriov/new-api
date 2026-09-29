package controller

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func TestResubmitTaskOnChannelBuildsChannelMetaBeforeUsingIt(t *testing.T) {
	task := &model.Task{
		TaskID:   "task_resubmit_channel_meta",
		UserId:   9,
		Group:    "default",
		Action:   constant.TaskActionTextToVideo,
		Platform: constant.TaskPlatform("unknown"),
	}
	task.Properties.OriginModelName = "gpt-image-2"
	require.True(t, task.SetResubmitPayload(
		[]byte(`{"model":"gpt-image-2","prompt":"a cat"}`),
		"application/json",
		"/v1/videos",
		256*1024,
	))

	channel := &model.Channel{
		Id:   3,
		Type: constant.ChannelTypeUnknown,
		Name: "backup",
		Key:  "test-key",
	}

	var err error
	require.NotPanics(t, func() {
		_, err = ResubmitTaskOnChannel(context.Background(), task, channel)
	})
	// 能走到「按新渠道类型解析插件」这一步、并且因为渠道类型未知而明确报错，
	// 说明渠道元数据初始化之前没有解引用（#11 那个空指针）。
	require.ErrorContains(t, err, "新渠道没有可用的任务插件")
}
