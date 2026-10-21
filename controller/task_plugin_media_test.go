package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mp4Header is the leading box of an MP4 file, which sniffs as video/mp4.
var mp4Header = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")

// soraUpstream serves the sora plugin content route the way our relay does:
// image results come back as PNG bytes labelled application/octet-stream.
type soraUpstream struct {
	server      *httptest.Server
	probeHits   atomic.Int32
	contentHits atomic.Int32
}

func newSoraUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *soraUpstream {
	t.Helper()
	upstream := &soraUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/videos/upstream_sora_1/content", r.URL.Path)
		assert.Equal(t, "Bearer provider-key", r.Header.Get("Authorization"))
		if r.Header.Get("Range") == "bytes=0-511" {
			upstream.probeHits.Add(1)
		} else {
			upstream.contentHits.Add(1)
		}
		handler(w, r)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// setupSoraPluginMediaTask stores a successful task executed by the factory
// sora plugin, whose listArtifacts always reports a single "video" artifact.
func setupSoraPluginMediaTask(t *testing.T, upstreamURL string) *model.Task {
	t.Helper()
	task := setupLegacyMediaTask(t, upstreamURL)
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCache })

	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Updates(map[string]any{
		"type":     constant.ChannelTypeSora,
		"key":      "provider-key",
		"base_url": upstreamURL,
	}).Error)
	task.Platform = constant.TaskPlatform("sora")
	task.Action = constant.TaskActionTextToVideo
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{
		Key: "sora", Name: "Sora", Version: "1.1.0", APIVersion: 1,
	}}
	task.PrivateData.UpstreamTaskID = "upstream_sora_1"
	// Polling stores the gateway's own proxy URL when the plugin reports none.
	task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
	task.SetData(map[string]any{
		"id": "upstream_sora_1", "object": "video", "status": "completed", "model": "gpt-image-2",
		"url": upstreamURL + "/files/abc", "image_url": upstreamURL + "/files/abc", "video_url": upstreamURL + "/files/abc",
	})
	require.NoError(t, model.DB.Save(task).Error)
	return task
}

func requestTaskArtifactContent(t *testing.T, task *model.Task) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(middleware.TaskArtifactAccessContextKey, true)
	c.Params = gin.Params{
		{Key: "key", Value: task.TaskID},
		{Key: "artifact_key", Value: "video"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.TaskID+"/artifacts/video/content", nil)
	TaskArtifactContent(c)
	return recorder
}

func TestDashboardTaskArtifactsDetectsPluginImageServedAsOctetStream(t *testing.T) {
	upstream := newSoraUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngHeader)
	})
	task := setupSoraPluginMediaTask(t, upstream.server.URL)

	response, body := requestDashboardLegacyArtifacts(t, task)

	require.Len(t, response.Data.Artifacts, 1)
	artifact := response.Data.Artifacts[0]
	assert.Equal(t, "video", artifact.Key, "the key stays so existing content URLs keep working")
	assert.Equal(t, "image", artifact.Type)
	assert.Equal(t, "image/png", artifact.MimeType)
	contentURL, err := url.Parse(artifact.ContentURL)
	require.NoError(t, err)
	assert.Equal(t, "/v1/tasks/"+task.TaskID+"/artifacts/video/content", contentURL.Path)
	assert.True(t, service.VerifyTaskArtifactAccess(
		contentURL.Query().Get(service.TaskArtifactAccessQueryParameter), task.TaskID, "video",
	))
	assert.Empty(t, response.Data.LegacyContentURL)
	assert.NotContains(t, body, upstream.server.URL)
	assert.NotContains(t, body, "upstream_sora_1")
	assert.NotContains(t, body, "provider-key")

	// A successful result is immutable, so the classification is reused.
	second, _ := requestDashboardLegacyArtifacts(t, task)
	require.Len(t, second.Data.Artifacts, 1)
	assert.Equal(t, "image", second.Data.Artifacts[0].Type)
	assert.Equal(t, int32(1), upstream.probeHits.Load())
	assert.Equal(t, int32(0), upstream.contentHits.Load())

	// The public artifacts endpoint shares the projection.
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId)
	c.Params = gin.Params{{Key: "key", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.TaskID+"/artifacts", nil)
	GetTaskArtifacts(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var public struct {
		Artifacts []taskArtifactResponse `json:"artifacts"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &public))
	require.Len(t, public.Artifacts, 1)
	assert.Equal(t, "image", public.Artifacts[0].Type)
	assert.Equal(t, "image/png", public.Artifacts[0].MimeType)
	assert.Equal(t, int32(1), upstream.probeHits.Load())
}

func TestDashboardTaskArtifactsKeepsPluginVideoAsVideo(t *testing.T) {
	upstream := newSoraUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(mp4Header)
	})
	task := setupSoraPluginMediaTask(t, upstream.server.URL)

	response, _ := requestDashboardLegacyArtifacts(t, task)

	require.Len(t, response.Data.Artifacts, 1)
	assert.Equal(t, "video", response.Data.Artifacts[0].Key)
	assert.Equal(t, "video", response.Data.Artifacts[0].Type)
	assert.Empty(t, response.Data.Artifacts[0].MimeType)
	assert.Equal(t, int32(1), upstream.probeHits.Load())
}

func TestDashboardTaskArtifactsKeepsPluginVideoServedAsOctetStream(t *testing.T) {
	upstream := newSoraUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(mp4Header)
	})
	task := setupSoraPluginMediaTask(t, upstream.server.URL)

	response, _ := requestDashboardLegacyArtifacts(t, task)

	require.Len(t, response.Data.Artifacts, 1)
	assert.Equal(t, "video", response.Data.Artifacts[0].Type)
	assert.Empty(t, response.Data.Artifacts[0].MimeType)
}

func TestDashboardTaskArtifactsFallsBackToVideoWhenPluginProbeFails(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	upstream := newSoraUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngHeader)
	})
	task := setupSoraPluginMediaTask(t, upstream.server.URL)

	response, _ := requestDashboardLegacyArtifacts(t, task)
	require.Len(t, response.Data.Artifacts, 1)
	assert.Equal(t, "video", response.Data.Artifacts[0].Type)
	assert.Empty(t, response.Data.Artifacts[0].MimeType)

	// A failed probe is not remembered; the next request detects the image.
	failing.Store(false)
	response, _ = requestDashboardLegacyArtifacts(t, task)
	require.Len(t, response.Data.Artifacts, 1)
	assert.Equal(t, "image", response.Data.Artifacts[0].Type)
	assert.Equal(t, int32(2), upstream.probeHits.Load())
}

func TestDashboardTaskArtifactsDoesNotProbeForeignPluginTask(t *testing.T) {
	upstream := newSoraUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngHeader)
	})
	task := setupSoraPluginMediaTask(t, upstream.server.URL)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId+1)
	c.Set("role", common.RoleCommonUser)
	c.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/task/"+task.TaskID+"/artifacts", nil)
	GetDashboardTaskArtifacts(c)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, int32(0), upstream.probeHits.Load())
}

func TestPluginImageArtifactContentIsServedAsDetectedImageType(t *testing.T) {
	upstream := newSoraUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngHeader)
	})
	task := setupSoraPluginMediaTask(t, upstream.server.URL)

	recorder := requestTaskArtifactContent(t, task)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "image/png", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, pngHeader, recorder.Body.Bytes())
	assert.Equal(t, int32(1), upstream.probeHits.Load())
	assert.Equal(t, int32(1), upstream.contentHits.Load())

	// The classification is cached, so later content requests only proxy.
	recorder = requestTaskArtifactContent(t, task)
	assert.Equal(t, "image/png", recorder.Header().Get("Content-Type"))
	assert.Equal(t, int32(1), upstream.probeHits.Load())
	assert.Equal(t, int32(2), upstream.contentHits.Load())
}

func TestPluginVideoArtifactContentKeepsUpstreamContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		probes      int32
	}{
		// A specific upstream type is kept without classifying the content.
		{"labelled video", "video/mp4", 0},
		{"generic video", "application/octet-stream", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newSoraUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write(mp4Header)
			})
			task := setupSoraPluginMediaTask(t, upstream.server.URL)

			recorder := requestTaskArtifactContent(t, task)

			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, tc.contentType, recorder.Header().Get("Content-Type"))
			assert.Equal(t, mp4Header, recorder.Body.Bytes())
			assert.Equal(t, tc.probes, upstream.probeHits.Load())
		})
	}
}

func TestGenericTaskMediaContentType(t *testing.T) {
	for _, contentType := range []string{"", "application/octet-stream", "binary/octet-stream", "Application/Octet-Stream; charset=binary", "not a type;;"} {
		assert.True(t, isGenericTaskMediaContentType(contentType), contentType)
	}
	for _, contentType := range []string{"video/mp4", "image/png", "application/json"} {
		assert.False(t, isGenericTaskMediaContentType(contentType), contentType)
	}
}
