package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这两个测试盯的是【钩子有没有真的接上】——也就是轮询判定任务失败时，
// 到底有没有走到换渠道重投那一步。
//
// 为什么单独写：上面那批单元测试全是直接调 TryResubmitTaskOnAnotherChannel 的，
// 把 task_polling.go 里那一行调用删掉，它们【依然全绿】。
// 真正会出事的恰恰是那一行没接上，所以必须有测试从轮询这一层进来。

// failingPollAdaptor 模拟一个「上游把任务判失败了」的轮询响应。
type failingPollAdaptor struct{ reason string }

func (a *failingPollAdaptor) Init(*relaycommon.RelayInfo) {}
func (a *failingPollAdaptor) FetchTask(string, string, map[string]any, string) (*http.Response, error) {
	// 故意不用 new-api 自己的响应格式，好让轮询走 adaptor.ParseTaskResult 那条路。
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"upstream":"whatever"}`))),
	}, nil
}
func (a *failingPollAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: a.reason}, nil
}
func (a *failingPollAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

func setupResubmitPollingTest(t *testing.T, failReason string) (*failingPollAdaptor, *model.Channel, *model.Task) {
	t.Helper()
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Ability{}))
	t.Cleanup(func() { model.DB.Exec("DELETE FROM abilities") })

	origEnabled := constant.TaskResubmitEnabled
	origMax := constant.TaskResubmitMaxAttempts
	origSkip := constant.TaskResubmitSkipReasons
	origFunc := ResubmitTaskFunc
	origMemCache := common.MemoryCacheEnabled
	constant.TaskResubmitEnabled = true
	constant.TaskResubmitMaxAttempts = 1
	constant.TaskResubmitSkipReasons = []string{"没有按照预期生成图片"}
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		constant.TaskResubmitEnabled = origEnabled
		constant.TaskResubmitMaxAttempts = origMax
		constant.TaskResubmitSkipReasons = origSkip
		ResubmitTaskFunc = origFunc
		common.MemoryCacheEnabled = origMemCache
	})

	// 对齐线上形态：主渠道优先级 10，备腿优先级 0，同分组同模型。
	for _, spec := range []struct {
		id       int
		priority int64
	}{{2, 10}, {3, 0}} {
		p := spec.priority
		w := uint(100)
		require.NoError(t, model.DB.Create(&model.Channel{
			Id: spec.id, Type: constant.ChannelTypeOpenAI, Name: "ch", Key: "sk-test",
			Status: common.ChannelStatusEnabled, Models: testResubmitModel,
			Group: testResubmitGroup, Priority: &p, Weight: &w,
		}).Error)
		require.NoError(t, model.DB.Create(&model.Ability{
			Group: testResubmitGroup, Model: testResubmitModel, ChannelId: spec.id,
			Enabled: true, Priority: &p, Weight: w,
		}).Error)
	}
	model.InitChannelCache()

	seedUser(t, 9, 0)
	seedToken(t, 90, 9, "sk-token", 0)
	seedChargedAccounting(t, 9, 2, 90, 1000, 1)

	task := &model.Task{
		TaskID: "task_poll_0001", Platform: constant.TaskPlatform("1"),
		UserId: 9, Group: testResubmitGroup, ChannelId: 2,
		Action: constant.TaskActionTextGenerate, Quota: 1000,
		Status: model.TaskStatus(model.TaskStatusInProgress), Progress: "30%",
		CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}
	task.Properties.OriginModelName = testResubmitModel
	task.PrivateData.UpstreamTaskID = "upstream-old"
	task.PrivateData.BillingSource = "wallet"
	task.PrivateData.TokenId = 90
	task.SetResubmitPayload([]byte(`{"model":"gpt-image-2","prompt":"a cat"}`),
		"application/json", testResubmitPath, 256*1024)
	task.MarkChannelTried(2)
	require.NoError(t, model.DB.Create(task).Error)

	var ch model.Channel
	require.NoError(t, model.DB.First(&ch, 2).Error)
	return &failingPollAdaptor{reason: failReason}, &ch, task
}

// ⭐ 主判据：上游侧失败 → 轮询这一层必须把任务换到另一个渠道，
//
//	不进终态、不退款。钩子没接上这条就红。
func TestPolling_UpstreamFailureIsResubmittedOnAnotherChannel(t *testing.T) {
	adaptor, ch, task := setupResubmitPollingTest(t, prodReasonUpstreamGlitch)

	var gotChannel int
	ResubmitTaskFunc = func(_ context.Context, _ *model.Task, c *model.Channel) (string, []byte, constant.TaskPlatform, error) {
		gotChannel = c.Id
		return "upstream-new", []byte(`{"id":"upstream-new"}`), constant.TaskPlatform("1"), nil
	}

	err := updateVideoSingleTask(context.Background(), adaptor, ch, task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task})
	require.NoError(t, err)
	assert.Equal(t, 3, gotChannel, "必须重投到没试过的渠道 3")

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, "task_id = ?", "task_poll_0001").Error)
	assert.Equal(t, 3, persisted.ChannelId, "换渠道必须真的落库（channel_id 不在快照比较范围里，最容易漏写）")
	assert.Equal(t, "upstream-new", persisted.GetUpstreamTaskID(), "新的上游任务 ID 必须落库")
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), persisted.Status, "任务还活着，不是终态失败")
	assert.Empty(t, persisted.FailReason)

	var user model.User
	require.NoError(t, model.DB.First(&user, 9).Error)
	assert.Equal(t, 1000, user.UsedQuota, "重投不退款——额度还占着，等最终结果")
	assert.Equal(t, 0, user.Quota, "钱包余额不该被退回")
}

// ⭐ 反向判据：内容审核类失败 → 照旧终态 + 退款。
//
//	没有这条，上面那条可以被「什么都重投」蒙混过去。
func TestPolling_ContentPolicyFailureStillGoesTerminalAndRefunds(t *testing.T) {
	adaptor, ch, task := setupResubmitPollingTest(t, prodReasonContentReject)

	called := false
	ResubmitTaskFunc = func(context.Context, *model.Task, *model.Channel) (string, []byte, constant.TaskPlatform, error) {
		called = true
		return "should-not-happen", nil, "", nil
	}

	err := updateVideoSingleTask(context.Background(), adaptor, ch, task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task})
	require.NoError(t, err)
	assert.False(t, called, "内容审核类失败不该重投——换家上游一样拒，重投只是把一次收费变两次")

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, "task_id = ?", "task_poll_0001").Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), persisted.Status)
	assert.Equal(t, 2, persisted.ChannelId, "渠道不该被改动")
	assert.Equal(t, prodReasonContentReject, persisted.FailReason)

	var user model.User
	require.NoError(t, model.DB.First(&user, 9).Error)
	assert.Equal(t, 0, user.UsedQuota, "终态失败必须照旧退款")
	assert.Equal(t, 1000, user.Quota, "钱退回钱包")
}
