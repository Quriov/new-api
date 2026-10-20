package controller

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pngHeader is the leading bytes of a PNG file, enough for content sniffing.
var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

type legacyArtifactsResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Artifacts         []taskArtifactResponse `json:"artifacts"`
		LegacyContentURL  string                 `json:"legacy_content_url"`
		LegacyContentType string                 `json:"legacy_content_type"`
		LegacyMimeType    string                 `json:"legacy_mime_type"`
	} `json:"data"`
}

func setupLegacyMediaTask(t *testing.T, upstreamURL string) *model.Task {
	t.Helper()
	task := setupGenericTaskTest(t)
	previousSecret := common.CryptoSecret
	previousPublicAddress := system_setting.TaskPublicAddress
	common.CryptoSecret = "controller-task-legacy-media-secret"
	system_setting.TaskPublicAddress = "https://gateway.example"
	legacyTaskMediaCache.Lock()
	legacyTaskMediaCache.items = make(map[string]legacyTaskMedia)
	legacyTaskMediaCache.Unlock()
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		system_setting.TaskPublicAddress = previousPublicAddress
		legacyTaskMediaCache.Lock()
		legacyTaskMediaCache.items = make(map[string]legacyTaskMedia)
		legacyTaskMediaCache.Unlock()
	})
	allowPrivateTaskMediaTest(t)
	// Image models are submitted through POST /v1/videos, so a successful image
	// task carries a video action and an extensionless upstream content URL.
	task.Platform = "openai-video"
	task.Action = "textGenerate"
	task.PrivateData.ResultURL = upstreamURL + "/v1/videos/upstream_task_1/content"
	require.NoError(t, model.DB.Save(task).Error)
	return task
}

func requestDashboardLegacyArtifacts(t *testing.T, task *model.Task) (legacyArtifactsResponse, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId)
	c.Set("role", common.RoleCommonUser)
	c.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/task/"+task.TaskID+"/artifacts", nil)

	GetDashboardTaskArtifacts(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response legacyArtifactsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	return response, recorder.Body.String()
}

func newLegacyMediaUpstream(t *testing.T, hits *atomic.Int32, handler func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "bytes=0-511", r.Header.Get("Range"))
		assert.Empty(t, r.Header.Get("Authorization"))
		handler(w)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestDashboardTaskArtifactsDetectsLegacyImageContent(t *testing.T) {
	var hits atomic.Int32
	upstream := newLegacyMediaUpstream(t, &hits, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(pngHeader)
	})
	task := setupLegacyMediaTask(t, upstream.URL)

	response, body := requestDashboardLegacyArtifacts(t, task)

	assert.Empty(t, response.Data.Artifacts)
	assert.Equal(t, "image", response.Data.LegacyContentType)
	assert.Equal(t, "image/png", response.Data.LegacyMimeType)
	contentURL, err := url.Parse(response.Data.LegacyContentURL)
	require.NoError(t, err)
	assert.Equal(t, "/v1/tasks/"+task.TaskID+"/artifacts/video/content", contentURL.Path)
	assert.True(t, service.VerifyTaskArtifactAccess(
		contentURL.Query().Get(service.TaskArtifactAccessQueryParameter), task.TaskID, "video",
	))
	assert.NotContains(t, body, upstream.URL)
	assert.NotContains(t, body, "upstream_task_1")

	// A successful result is immutable, so the classification is reused.
	second, _ := requestDashboardLegacyArtifacts(t, task)
	assert.Equal(t, "image", second.Data.LegacyContentType)
	assert.Equal(t, int32(1), hits.Load())
}

func TestDashboardTaskArtifactsKeepsLegacyVideoContentAsVideo(t *testing.T) {
	var hits atomic.Int32
	upstream := newLegacyMediaUpstream(t, &hits, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-511/4096")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 512))
	})
	task := setupLegacyMediaTask(t, upstream.URL)

	response, _ := requestDashboardLegacyArtifacts(t, task)

	assert.Equal(t, "video", response.Data.LegacyContentType)
	assert.Equal(t, "video/mp4", response.Data.LegacyMimeType)
	assert.NotEmpty(t, response.Data.LegacyContentURL)
	assert.Equal(t, int32(1), hits.Load())
}

func TestDashboardTaskArtifactsSniffsGenericLegacyContentType(t *testing.T) {
	var hits atomic.Int32
	upstream := newLegacyMediaUpstream(t, &hits, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngHeader)
	})
	task := setupLegacyMediaTask(t, upstream.URL)

	response, _ := requestDashboardLegacyArtifacts(t, task)

	assert.Equal(t, "image", response.Data.LegacyContentType)
	assert.Equal(t, "image/png", response.Data.LegacyMimeType)
}

func TestDashboardTaskArtifactsFallsBackToVideoWhenLegacyProbeFails(t *testing.T) {
	var hits atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	upstream := newLegacyMediaUpstream(t, &hits, func(w http.ResponseWriter) {
		if failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		w.WriteHeader(http.StatusPartialContent)
	})
	task := setupLegacyMediaTask(t, upstream.URL)

	response, _ := requestDashboardLegacyArtifacts(t, task)
	assert.Equal(t, "video", response.Data.LegacyContentType)
	assert.Empty(t, response.Data.LegacyMimeType)
	assert.NotEmpty(t, response.Data.LegacyContentURL)

	// A failed probe is not remembered; the next request detects the image.
	failing.Store(false)
	response, _ = requestDashboardLegacyArtifacts(t, task)
	assert.Equal(t, "image", response.Data.LegacyContentType)
	assert.Equal(t, int32(2), hits.Load())
}

func TestPublicTaskArtifactsReportsLegacyContentType(t *testing.T) {
	var hits atomic.Int32
	upstream := newLegacyMediaUpstream(t, &hits, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusPartialContent)
	})
	task := setupLegacyMediaTask(t, upstream.URL)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", task.UserId)
	c.Params = gin.Params{{Key: "key", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.TaskID+"/artifacts", nil)

	GetTaskArtifacts(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		LegacyContentURL  string `json:"legacy_content_url"`
		LegacyContentType string `json:"legacy_content_type"`
		LegacyMimeType    string `json:"legacy_mime_type"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.NotEmpty(t, response.LegacyContentURL)
	assert.Equal(t, "image", response.LegacyContentType)
	assert.Equal(t, "image/jpeg", response.LegacyMimeType)
}

func TestLegacyTaskMediaClassification(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		want        legacyTaskMedia
	}{
		{"image with parameters", "image/png; charset=binary", legacyTaskMedia{Type: "image", MimeType: "image/png"}},
		{"video", "video/webm", legacyTaskMedia{Type: "video", MimeType: "video/webm"}},
		{"generic", "application/octet-stream", legacyTaskMedia{}},
		{"empty", "", legacyTaskMedia{}},
		{"malformed", "image/png\r\nX-Injected: 1", legacyTaskMedia{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, legacyTaskMediaFromContentType(tc.contentType))
		})
	}

	assert.Equal(t,
		legacyTaskMedia{Type: "image", MimeType: "image/jpeg"},
		legacyTaskMediaFromDataURL("data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString([]byte("x"))),
	)
	assert.Equal(t,
		legacyTaskMedia{Type: "image", MimeType: "image/png"},
		legacyTaskMediaFromDataURL("data:;base64,"+base64.StdEncoding.EncodeToString(pngHeader)),
	)
	assert.Equal(t, legacyTaskMedia{}, legacyTaskMediaFromDataURL("data:,plain"))
}
