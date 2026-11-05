package controller

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Fixture copied verbatim from Quriov-AI-Video@1318d98cb30d3e18d54dc009175b4825d7050ebf:
// scripts/newapi/catalog-2026-10/batch-20261106-seconds-relist/plugin/sora.js.
// Keep the real Geeknow base URL for the plugin's host-specific conversion,
// but redirect every HTTP dial to a local mock. No external upstream is contacted.
func TestVideoSecondsUnclaimedSoraFullSubmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	source, err := os.ReadFile("testdata/sora_seconds_quriov.js")
	require.NoError(t, err)
	previousRegistry := pluginruntime.DefaultRegistry
	registry := pluginruntime.NewRegistry()
	pluginruntime.DefaultRegistry = registry
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	plugin, err := registry.Register(string(source), pluginruntime.Options{})
	require.NoError(t, err)
	require.NotContains(t, plugin.Meta.Models, "Vidu-q2")
	_, claimed := registry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", "Vidu-q2")
	require.False(t, claimed)

	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousCache, previousBatch := common.MemoryCacheEnabled, common.BatchUpdateEnabled
	previousConsume, previousRedis := common.LogConsumeEnabled, common.RedisEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Channel{}, &model.Task{}, &model.Log{}))
	model.DB, model.LOG_DB = database, database
	common.MemoryCacheEnabled, common.BatchUpdateEnabled = false, false
	common.LogConsumeEnabled, common.RedisEnabled = false, false
	previousPrices := ratio_setting.ModelPrice2JSONString()
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"Vidu-q2":0.01}`))
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.MemoryCacheEnabled, common.BatchUpdateEnabled = previousCache, previousBatch
		common.LogConsumeEnabled, common.RedisEnabled = previousConsume, previousRedis
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices))
		sqlDB, err := database.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, database.Create(&model.User{Id: 7, Username: "seconds-test", Group: "default", Quota: 1000000}).Error)

	var sent []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/videos", r.URL.Path)
		var readErr error
		sent, readErr = io.ReadAll(r.Body)
		assert.NoError(t, readErr)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"mock-video-5","status":"queued","seconds":"5"}`)
	}))
	defer upstream.Close()
	service.InitHttpClient()
	client := service.GetHttpClient()
	previousTransport := client.Transport
	mockTransport := http.DefaultTransport.(*http.Transport).Clone()
	mockTransport.Proxy = nil
	// Dial only the mock's loopback address, including when the URL says Geeknow.
	mockTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	client.Transport = mockTransport
	t.Cleanup(func() { client.Transport = previousTransport; mockTransport.CloseIdleConnections() })
	baseURL := "http://www.geeknow.top"
	channel := model.Channel{Type: 1, Name: "seconds-local", Key: "dummy", BaseURL: &baseURL, Status: common.ChannelStatusEnabled, Models: "Vidu-q2", Group: "default"}
	require.NoError(t, database.Create(&channel).Error)
	var billedSeconds float64
	engine := gin.New()
	engine.POST("/v1/videos", middleware.PinTaskPluginEndpoint(), func(c *gin.Context) {
		defer common.CleanupBodyStorage(c)
		_, pinned := c.Get(pluginruntime.ContextKeyPinnedEndpoint)
		require.False(t, pinned, "unclaimed model must use legacy request parser")
		common.SetContextKey(c, constant.ContextKeyUserId, 7)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserQuota, 1000000)
		require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, "Vidu-q2"))
		info := &relaycommon.RelayInfo{UserId: 7, UserGroup: "default", UsingGroup: "default", TokenGroup: "default", UserQuota: 1000000, OriginModelName: "Vidu-q2", Billing: &nativeRouteBilling{userID: 7}, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_seconds_public", LockedChannel: &channel}}
		outcome, taskErr := executeTaskSubmissionWith(c, info, relay.RelayTaskSubmit)
		require.Nil(t, taskErr, "%+v", taskErr)
		require.NotNil(t, outcome)
		pinnedPlugin, ok := c.MustGet(pluginruntime.ContextKeyPinnedPlugin).(pluginruntime.PinnedPlugin)
		require.True(t, ok)
		require.Same(t, plugin, pinnedPlugin.Plugin)
		require.Equal(t, 1, info.ChannelType)
		billedSeconds = info.PriceData.OtherRatios()["seconds"]
		presentTaskSubmission(c, outcome)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewBufferString(`{"model":"Vidu-q2","prompt":"a lighthouse","seconds":5}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var body map[string]any
	require.NoError(t, common.Unmarshal(sent, &body))
	require.Equal(t, "5", body["seconds"])
	require.Equal(t, float64(5), billedSeconds)
	t.Logf("unclaimed=true channel_type=1 plugin=sora input_seconds=5 gateway_status=%d upstream_seconds=%q billing_seconds=%g", recorder.Code, body["seconds"], billedSeconds)
	t.Logf("upstream_body=%s", sent)
}
