package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 这些测试盯的是一个真实事故：2026-08-21 那天 28 次任务失败【全部】落在同一个渠道上，
// 旁边配好并且启用着的备用渠道一次都没有被试过 —— 因为 new-api 的渠道重试
// 只覆盖提交阶段，任务一旦被上游收下就再也不会换腿。
//
// 两个方向都要钉住：
//   - 上游侧失败（网络错误之类）→ 必须换渠道重投
//   - 内容审核类失败（换家上游一样拒）→ 必须【不】重投，否则一次收费变两次

const (
	testResubmitModel = "gpt-image-2"
	testResubmitGroup = "default"
	testResubmitPath  = "/v1/videos"
	// 线上真实出现过的两种失败原因，直接拿来当判据的输入。
	prodReasonUpstreamGlitch = "Internet Error，请耐心等待！"
	prodReasonContentReject  = "没有按照预期生成图片，请重新调整提示词后重试"
)

func setupResubmitTest(t *testing.T) *gorm.DB {
	t.Helper()

	origDB := model.DB
	origMemCache := common.MemoryCacheEnabled
	origEnabled := constant.TaskResubmitEnabled
	origMax := constant.TaskResubmitMaxAttempts
	origSkip := constant.TaskResubmitSkipReasons
	origFunc := ResubmitTaskFunc

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	common.MemoryCacheEnabled = true

	constant.TaskResubmitEnabled = true
	constant.TaskResubmitMaxAttempts = 1
	constant.TaskResubmitSkipReasons = []string{"没有按照预期生成图片", "content_policy", "safety system"}

	t.Cleanup(func() {
		model.DB = origDB
		common.MemoryCacheEnabled = origMemCache
		constant.TaskResubmitEnabled = origEnabled
		constant.TaskResubmitMaxAttempts = origMax
		constant.TaskResubmitSkipReasons = origSkip
		ResubmitTaskFunc = origFunc
		if origMemCache && origDB != nil &&
			origDB.Migrator().HasTable(&model.Channel{}) && origDB.Migrator().HasTable(&model.Ability{}) {
			model.InitChannelCache()
		}
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// seedResubmitChannel 造一个带指定优先级的渠道 —— 对齐线上形态：
// 主渠道优先级 10、备腿优先级 0。
func seedResubmitChannel(t *testing.T, db *gorm.DB, id int, priority int64) {
	t.Helper()
	weight := uint(100)
	require.NoError(t, db.Create(&model.Channel{
		Id:       id,
		Type:     constant.ChannelTypeOpenAI,
		Key:      fmt.Sprintf("key-%d", id),
		Status:   common.ChannelStatusEnabled,
		Name:     fmt.Sprintf("channel-%d", id),
		Weight:   &weight,
		Models:   testResubmitModel,
		Group:    testResubmitGroup,
		Priority: &priority,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group:     testResubmitGroup,
		Model:     testResubmitModel,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
}

// newFailedTask 造一个「已经在渠道 onChannel 上提交成功、刚被上游判失败」的任务。
func newFailedTask(onChannel int) *model.Task {
	task := &model.Task{
		TaskID:    "task_test_0001",
		UserId:    9,
		Group:     testResubmitGroup,
		ChannelId: onChannel,
		Platform:  constant.TaskPlatform("1"),
		Action:    constant.TaskActionTextGenerate,
		Quota:     1000,
		// ⚠ 必须是 FAILURE，不能图省事写 IN_PROGRESS：
		//   轮询代码在进失败分支【之前】就已经把 task.Status 置成上游返回的
		//   FAILURE 了，我们是在那之后被调用的。夹具写成 IN_PROGRESS 的话，
		//   「重投后要把状态改回在跑」这条断言会恒真（变异测试就是这么抓到的）。
		Status:     model.TaskStatusFailure,
		FailReason: prodReasonUpstreamGlitch,
		FinishTime: 1755800000,
	}
	task.Properties.OriginModelName = testResubmitModel
	task.PrivateData.UpstreamTaskID = "upstream-old"
	task.SetResubmitPayload([]byte(`{"model":"gpt-image-2","prompt":"a cat"}`),
		"application/json", testResubmitPath, 256*1024)
	task.MarkChannelTried(onChannel)
	return task
}

// ── 核心：上游侧失败要真的换腿 ────────────────────────────────────────

func TestResubmit_SwitchesToAnotherChannelWhenUpstreamFails(t *testing.T) {
	db := setupResubmitTest(t)
	seedResubmitChannel(t, db, 2, 10) // 主渠道（线上 zexapi）
	seedResubmitChannel(t, db, 3, 0)  // 备腿（线上 otuapi），从来没被试过
	model.InitChannelCache()

	var calledOnChannel int
	ResubmitTaskFunc = func(_ context.Context, _ *model.Task, ch *model.Channel) (string, []byte, constant.TaskPlatform, error) {
		calledOnChannel = ch.Id
		return "upstream-new", []byte(`{"id":"upstream-new"}`), constant.TaskPlatform("1"), nil
	}

	task := newFailedTask(2)
	ok := TryResubmitTaskOnAnotherChannel(context.Background(), task, prodReasonUpstreamGlitch)

	require.True(t, ok, "上游侧失败必须触发换渠道重投")
	assert.Equal(t, 3, calledOnChannel, "必须打到没试过的那个渠道，而不是原来那个")
	assert.Equal(t, 3, task.ChannelId, "任务的渠道要跟着换过去")
	assert.Equal(t, "upstream-new", task.PrivateData.UpstreamTaskID, "要跟踪新的上游任务 ID")
	// ⚠ 上游 const 块里只有第一行带类型，TaskStatusInProgress 其实是【无类型字符串常量】，
	//   直接跟 TaskStatus 比会因为类型不同判不等。这里显式转一下。
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), task.Status, "重投后任务还活着，不是终态")
	assert.Empty(t, task.FailReason, "重投后不该留着上一次的失败原因")
	assert.Zero(t, task.FinishTime, "重投后任务没结束，完成时间要清掉")
	assert.Equal(t, "0%", task.Progress, "重投后进度要归零，不能留着上一次的 100%")
	assert.Equal(t, 1, task.PrivateData.ResubmitCount)
	assert.ElementsMatch(t, []int{2, 3}, task.TriedChannelIDs(), "两个渠道都要记进已试清单")
	assert.Equal(t, "task_test_0001", task.TaskID, "客户手上的 task_id 不能变")
}

// ── 反向：内容审核类失败不许重投（换家上游一样拒，重投只是把一次收费变两次）──

func TestResubmit_ContentPolicyFailureIsNotRetried(t *testing.T) {
	db := setupResubmitTest(t)
	seedResubmitChannel(t, db, 2, 10)
	seedResubmitChannel(t, db, 3, 0)
	model.InitChannelCache()

	called := false
	ResubmitTaskFunc = func(_ context.Context, _ *model.Task, _ *model.Channel) (string, []byte, constant.TaskPlatform, error) {
		called = true
		return "should-not-happen", nil, "", nil
	}

	task := newFailedTask(2)
	ok := TryResubmitTaskOnAnotherChannel(context.Background(), task, prodReasonContentReject)

	require.False(t, ok, "内容审核类失败不该重投")
	assert.False(t, called, "根本不该发出重投请求")
	assert.Equal(t, 2, task.ChannelId, "渠道不该被改动")
	assert.Equal(t, 0, task.PrivateData.ResubmitCount)
}

func TestDecideResubmit_SkipListIsCaseInsensitive(t *testing.T) {
	setupResubmitTest(t)
	task := newFailedTask(2)

	assert.False(t, DecideResubmit(task, "Request rejected by CONTENT_POLICY filter").ShouldResubmit,
		"上游的英文报错大小写不统一，匹配必须大小写不敏感")
	assert.True(t, DecideResubmit(task, prodReasonUpstreamGlitch).ShouldResubmit,
		"网络类失败不在清单里，必须允许重投")
}

func TestDecideResubmit_EmptyReasonIsTreatedAsUpstreamSideFailure(t *testing.T) {
	setupResubmitTest(t)
	task := newFailedTask(2)
	assert.True(t, DecideResubmit(task, "").ShouldResubmit,
		"上游没给失败原因时按上游侧问题处理，允许换腿")
}

// ── 边界 ──────────────────────────────────────────────────────────────

func TestDecideResubmit_StopsAtMaxAttempts(t *testing.T) {
	setupResubmitTest(t)
	constant.TaskResubmitMaxAttempts = 2

	task := newFailedTask(2)
	task.PrivateData.ResubmitCount = 1
	assert.True(t, DecideResubmit(task, prodReasonUpstreamGlitch).ShouldResubmit, "还没到上限")

	task.PrivateData.ResubmitCount = 2
	d := DecideResubmit(task, prodReasonUpstreamGlitch)
	assert.False(t, d.ShouldResubmit, "到上限就必须停")
	assert.Contains(t, d.Reason, "上限")
}

func TestDecideResubmit_DeclinesWithoutStoredPayload(t *testing.T) {
	setupResubmitTest(t)
	task := newFailedTask(2)
	task.PrivateData.ResubmitBodyB64 = "" // 老任务 / 请求体过大没留存
	d := DecideResubmit(task, prodReasonUpstreamGlitch)
	assert.False(t, d.ShouldResubmit)
	assert.Contains(t, d.Reason, "原始请求")
}

func TestDecideResubmit_DisabledByConfig(t *testing.T) {
	setupResubmitTest(t)
	constant.TaskResubmitEnabled = false
	task := newFailedTask(2)
	assert.False(t, DecideResubmit(task, prodReasonUpstreamGlitch).ShouldResubmit)
}

func TestResubmit_DeclinesWhenNoOtherChannelAvailable(t *testing.T) {
	db := setupResubmitTest(t)
	seedResubmitChannel(t, db, 2, 10) // 只有一条腿
	model.InitChannelCache()

	called := false
	ResubmitTaskFunc = func(_ context.Context, _ *model.Task, _ *model.Channel) (string, []byte, constant.TaskPlatform, error) {
		called = true
		return "x", nil, "", nil
	}

	task := newFailedTask(2)
	ok := TryResubmitTaskOnAnotherChannel(context.Background(), task, prodReasonUpstreamGlitch)

	require.False(t, ok, "没有第二条腿就不该假装重投成功")
	assert.False(t, called, "不该把请求再打给同一个已经失败的渠道")
	assert.Equal(t, 2, task.ChannelId)
}

func TestResubmit_MarksChannelTriedWhenResubmitCallItselfFails(t *testing.T) {
	db := setupResubmitTest(t)
	seedResubmitChannel(t, db, 2, 10)
	seedResubmitChannel(t, db, 3, 0)
	model.InitChannelCache()

	ResubmitTaskFunc = func(_ context.Context, _ *model.Task, _ *model.Channel) (string, []byte, constant.TaskPlatform, error) {
		return "", nil, "", fmt.Errorf("上游 503")
	}

	task := newFailedTask(2)
	ok := TryResubmitTaskOnAnotherChannel(context.Background(), task, prodReasonUpstreamGlitch)

	require.False(t, ok)
	assert.ElementsMatch(t, []int{2, 3}, task.TriedChannelIDs(),
		"重投本身失败的渠道也要记进已试清单，否则下一轮会反复挑中同一个坏渠道")
	assert.Equal(t, 2, task.ChannelId, "重投没成功，任务还留在原渠道上")
}

func TestResubmit_FollowsNewChannelPlatformSoPollingUsesRightAdaptor(t *testing.T) {
	db := setupResubmitTest(t)
	seedResubmitChannel(t, db, 2, 10)
	seedResubmitChannel(t, db, 3, 0)
	model.InitChannelCache()

	ResubmitTaskFunc = func(_ context.Context, _ *model.Task, _ *model.Channel) (string, []byte, constant.TaskPlatform, error) {
		return "upstream-new", nil, constant.TaskPlatform("33"), nil
	}

	task := newFailedTask(2)
	require.True(t, TryResubmitTaskOnAnotherChannel(context.Background(), task, prodReasonUpstreamGlitch))
	assert.Equal(t, constant.TaskPlatform("33"), task.Platform,
		"轮询是按 platform 挑适配器的，换了不同类型的渠道就必须跟着改，否则下一轮拿错适配器")
}

// ── 渠道挑选 ──────────────────────────────────────────────────────────

func TestPickResubmitChannel_SkipsTriedChannels(t *testing.T) {
	db := setupResubmitTest(t)
	seedResubmitChannel(t, db, 2, 10)
	seedResubmitChannel(t, db, 3, 0)
	model.InitChannelCache()

	ch, err := PickResubmitChannel(testResubmitGroup, testResubmitModel, testResubmitPath, []int{2})
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.Equal(t, 3, ch.Id)

	ch, err = PickResubmitChannel(testResubmitGroup, testResubmitModel, testResubmitPath, []int{2, 3})
	require.NoError(t, err)
	assert.Nil(t, ch, "全试过了就该返回 nil，而不是又给一个试过的")
}

// ── Task 上的读写辅助 ─────────────────────────────────────────────────

func TestTriedChannelIDs_LegacyTaskFallsBackToCurrentChannel(t *testing.T) {
	task := &model.Task{ChannelId: 7}
	assert.Equal(t, []int{7}, task.TriedChannelIDs(),
		"本功能上线前的老任务没有这个字段，至少当前渠道算试过")
}

func TestMarkChannelTried_IsIdempotent(t *testing.T) {
	task := &model.Task{ChannelId: 2}
	task.MarkChannelTried(2)
	task.MarkChannelTried(3)
	task.MarkChannelTried(3)
	assert.ElementsMatch(t, []int{2, 3}, task.TriedChannelIDs())
	assert.Equal(t, 2, strings.Count(task.PrivateData.TriedChannels, ",")+1,
		"重复标记不该在字段里堆出重复 ID")
}

func TestSetResubmitPayload_RejectsOversizedBody(t *testing.T) {
	task := &model.Task{}
	big := make([]byte, 1024)
	assert.False(t, task.SetResubmitPayload(big, "application/json", testResubmitPath, 512),
		"超过上限不该留存 —— 宁可放弃重投也不要把大请求体塞进 tasks 表")
	_, ok := task.ResubmitPayload()
	assert.False(t, ok)

	assert.True(t, task.SetResubmitPayload([]byte("small"), "application/json", testResubmitPath, 512))
	body, ok := task.ResubmitPayload()
	require.True(t, ok)
	assert.Equal(t, []byte("small"), body, "存进去再取出来必须是原字节")
}

func TestResubmitPayload_SurvivesBinaryBody(t *testing.T) {
	task := &model.Task{}
	// multipart 里夹着图片时请求体不是合法 UTF-8，直接塞进 JSON 字符串会被改写坏，
	// 所以留存走 base64。这条钉住这个决定。
	binary := []byte{0x00, 0xff, 0xfe, 0x80, 0x41}
	require.True(t, task.SetResubmitPayload(binary, "multipart/form-data", testResubmitPath, 1024))
	got, ok := task.ResubmitPayload()
	require.True(t, ok)
	assert.Equal(t, binary, got)
}
