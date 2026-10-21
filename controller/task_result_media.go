package controller

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/gin-gonic/gin"
)

// Image models are submitted through the video endpoints too, so neither the
// task action nor the artifact a plugin projects can tell images from videos:
// the sora plugin, for example, always lists its result as a "video" artifact,
// and the upstream serves image results as application/octet-stream from an
// extensionless URL. The artifacts endpoint therefore inspects the content
// itself — the upstream Content-Type, or the leading bytes when that header is
// missing or generic — so the dashboard can render the right preview, and the
// content endpoint can label image bytes so <img> renders under nosniff.

const (
	taskResultMediaTypeImage = "image"
	taskResultMediaTypeVideo = "video"

	taskResultMediaSniffBytes    = 512
	taskResultMediaCacheCapacity = 4096

	// maxProbedTaskArtifacts bounds how many untyped plugin video artifacts a
	// single artifacts request classifies; the rest keep the video preview.
	maxProbedTaskArtifacts = 4
)

var taskResultMediaProbeTimeout = 10 * time.Second

type taskResultMedia struct {
	Type     string
	MimeType string
}

func (m taskResultMedia) isImage() bool {
	return m.Type == taskResultMediaTypeImage && m.MimeType != ""
}

// taskResultMediaCache remembers successful classifications. Task results are
// immutable once successful, so entries never go stale; the cache key carries
// the artifact key and the result URL so a changed result is probed again. The
// map is cleared when it reaches capacity, which only costs one more upstream
// probe per task.
var taskResultMediaCache = struct {
	sync.Mutex
	items map[string]taskResultMedia
}{items: make(map[string]taskResultMedia)}

func taskResultMediaCacheKey(task *model.Task, artifactKey string) string {
	return task.TaskID + "\x00" + artifactKey + "\x00" + task.GetResultURL()
}

func loadTaskResultMediaCache(key string) (taskResultMedia, bool) {
	taskResultMediaCache.Lock()
	defer taskResultMediaCache.Unlock()
	media, ok := taskResultMediaCache.items[key]
	return media, ok
}

func storeTaskResultMediaCache(key string, media taskResultMedia) {
	taskResultMediaCache.Lock()
	defer taskResultMediaCache.Unlock()
	if len(taskResultMediaCache.items) >= taskResultMediaCacheCapacity {
		taskResultMediaCache.items = make(map[string]taskResultMedia)
	}
	taskResultMediaCache.items[key] = media
}

// resolveTaskResultMedia reports whether a task result artifact is an image or
// a video. A non-plugin task only has the "video" artifact backed by its result
// URL. Anything it cannot classify keeps the historical video preview.
func resolveTaskResultMedia(c *gin.Context, task *model.Task, artifactKey string) taskResultMedia {
	// A discarded result is never served by the content endpoint, so there is
	// nothing to preview and no reason to contact the upstream.
	if task == nil || !task.ResultRetrievable() {
		return taskResultMedia{Type: taskResultMediaTypeVideo}
	}
	key := taskResultMediaCacheKey(task, artifactKey)
	if media, ok := loadTaskResultMediaCache(key); ok {
		return media
	}
	media, err := probeTaskResultMedia(c, task, artifactKey)
	if err != nil {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("Failed to detect result media type for task %s: %v", task.TaskID, err))
		return taskResultMedia{Type: taskResultMediaTypeVideo}
	}
	if media.Type == "" {
		return taskResultMedia{Type: taskResultMediaTypeVideo}
	}
	storeTaskResultMediaCache(key, media)
	return media
}

func probeTaskResultMedia(c *gin.Context, task *model.Task, artifactKey string) (taskResultMedia, error) {
	clientHeaders := map[string]string{
		"Range": fmt.Sprintf("bytes=0-%d", taskResultMediaSniffBytes-1),
	}
	rawURL := strings.TrimSpace(task.GetResultURL())
	var descriptor *relaychannel.TaskContentRequest
	switch {
	case strings.HasPrefix(rawURL, "data:"):
		return taskResultMediaFromDataURL(rawURL), nil
	case rawURL != "" && !isTaskMediaFallbackLoop(rawURL, task.TaskID):
		descriptor = &relaychannel.TaskContentRequest{
			URL:            rawURL,
			Method:         http.MethodGet,
			Credentialless: true,
		}
	case taskHasPluginExecution(task):
		// The result URL is missing or only points back at this gateway, so
		// probe the same upstream request the content endpoint proxies.
		pluginDescriptor, err := buildPluginTaskContentRequest(task, artifactKey, relaychannel.TaskArtifactClientRequest{
			Method:  http.MethodGet,
			Headers: clientHeaders,
		})
		if err != nil {
			return taskResultMedia{}, err
		}
		if pluginURL := strings.TrimSpace(pluginDescriptor.URL); strings.HasPrefix(pluginURL, "data:") {
			return taskResultMediaFromDataURL(pluginURL), nil
		}
		descriptor = pluginDescriptor
	default:
		return taskResultMedia{}, errTaskMediaRequestRejected
	}
	resp, err := openTaskMedia(c, task, descriptor, clientHeaders, taskResultMediaProbeTimeout)
	if err != nil {
		return taskResultMedia{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return taskResultMedia{}, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	if media := taskResultMediaFromContentType(resp.Header.Get("Content-Type")); media.Type != "" {
		return media, nil
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, taskResultMediaSniffBytes))
	if err != nil && len(head) == 0 {
		return taskResultMedia{}, err
	}
	return taskResultMediaFromContentType(http.DetectContentType(head)), nil
}

// buildPluginTaskContentRequest asks the task's plugin for the upstream request
// that serves one artifact, exactly as the content endpoint does.
func buildPluginTaskContentRequest(
	task *model.Task,
	artifactKey string,
	clientRequest relaychannel.TaskArtifactClientRequest,
) (*relaychannel.TaskContentRequest, error) {
	adaptor, err := initTaskArtifactAdaptor(task)
	if err != nil {
		return nil, err
	}
	provider, ok := adaptor.(relaychannel.TaskContentRequestProvider)
	if !ok {
		return nil, errTaskArtifactPluginUnavailable
	}
	descriptor, err := provider.BuildContentRequest(task, artifactKey, clientRequest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errTaskArtifactPlugin, err)
	}
	if descriptor == nil {
		return nil, errTaskArtifactPlugin
	}
	return descriptor, nil
}

// pluginArtifactNeedsMediaProbe reports whether a projected plugin artifact
// only claims the generic video kind, which image models also produce.
func pluginArtifactNeedsMediaProbe(artifact relaychannel.TaskArtifact) bool {
	return artifact.Type == taskResultMediaTypeVideo && strings.TrimSpace(artifact.MimeType) == ""
}

// isGenericTaskMediaContentType reports whether an upstream Content-Type says
// nothing about the media kind, so a detected image type may replace it.
func isGenericTaskMediaContentType(contentType string) bool {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return true
	}
	switch mediaType {
	case "application/octet-stream", "binary/octet-stream":
		return true
	default:
		return false
	}
}

// taskResultMediaFromContentType classifies an image/* or video/* media type.
// Generic or unknown types return an empty result so the caller can sniff.
func taskResultMediaFromContentType(contentType string) taskResultMedia {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || len(mediaType) > 255 {
		return taskResultMedia{}
	}
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return taskResultMedia{Type: taskResultMediaTypeImage, MimeType: mediaType}
	case strings.HasPrefix(mediaType, "video/"):
		return taskResultMedia{Type: taskResultMediaTypeVideo, MimeType: mediaType}
	default:
		return taskResultMedia{}
	}
}

func taskResultMediaFromDataURL(dataURL string) taskResultMedia {
	header, payload, found := strings.Cut(dataURL, ",")
	if !found {
		return taskResultMedia{}
	}
	mediaType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if media := taskResultMediaFromContentType(mediaType); media.Type != "" {
		return media
	}
	if !strings.HasSuffix(header, ";base64") {
		return taskResultMedia{}
	}
	// Decode just enough of the payload to sniff the leading bytes.
	encodedPrefix := payload
	if limit := base64.StdEncoding.EncodedLen(taskResultMediaSniffBytes); len(encodedPrefix) > limit {
		encodedPrefix = encodedPrefix[:limit]
	}
	head, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encodedPrefix)))
	if len(head) == 0 {
		return taskResultMedia{}
	}
	return taskResultMediaFromContentType(http.DetectContentType(head))
}
