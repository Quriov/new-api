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
		Action:   constant.TaskActionTextGenerate,
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
		_, _, _, err = ResubmitTaskOnChannel(context.Background(), task, channel)
	})
	require.ErrorContains(t, err, "找不到平台")
}
