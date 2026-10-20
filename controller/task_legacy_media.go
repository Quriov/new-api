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

// Non-plugin task results are a single upstream URL with no declared media
// kind. Image models are submitted through the video endpoints too, so the
// task action cannot tell images from videos and the result URL usually has
// no file extension. The artifacts endpoint therefore inspects the content
// itself — the upstream Content-Type, or the leading bytes when that header
// is missing or generic — so the dashboard can render the right preview.

const (
	legacyTaskMediaTypeImage = "image"
	legacyTaskMediaTypeVideo = "video"

	legacyTaskMediaSniffBytes    = 512
	legacyTaskMediaCacheCapacity = 4096
)

var legacyTaskMediaProbeTimeout = 10 * time.Second

type legacyTaskMedia struct {
	Type     string
	MimeType string
}

// legacyTaskMediaCache remembers successful classifications. Task results are
// immutable once successful, so entries never go stale; the cache key carries
// the result URL so a changed result is probed again. The map is cleared when
// it reaches capacity, which only costs one more upstream probe per task.
var legacyTaskMediaCache = struct {
	sync.Mutex
	items map[string]legacyTaskMedia
}{items: make(map[string]legacyTaskMedia)}

func legacyTaskMediaCacheKey(task *model.Task) string {
	return task.TaskID + "\x00" + task.GetResultURL()
}

func loadLegacyTaskMediaCache(key string) (legacyTaskMedia, bool) {
	legacyTaskMediaCache.Lock()
	defer legacyTaskMediaCache.Unlock()
	media, ok := legacyTaskMediaCache.items[key]
	return media, ok
}

func storeLegacyTaskMediaCache(key string, media legacyTaskMedia) {
	legacyTaskMediaCache.Lock()
	defer legacyTaskMediaCache.Unlock()
	if len(legacyTaskMediaCache.items) >= legacyTaskMediaCacheCapacity {
		legacyTaskMediaCache.items = make(map[string]legacyTaskMedia)
	}
	legacyTaskMediaCache.items[key] = media
}

// resolveLegacyTaskMedia reports whether a non-plugin task result is an image
// or a video. Anything it cannot classify keeps the historical video preview.
func resolveLegacyTaskMedia(c *gin.Context, task *model.Task) legacyTaskMedia {
	key := legacyTaskMediaCacheKey(task)
	if media, ok := loadLegacyTaskMediaCache(key); ok {
		return media
	}
	media, err := probeLegacyTaskMedia(c, task)
	if err != nil {
		logger.LogWarn(c.Request.Context(), fmt.Sprintf("Failed to detect legacy media type for task %s: %v", task.TaskID, err))
		return legacyTaskMedia{Type: legacyTaskMediaTypeVideo}
	}
	if media.Type == "" {
		return legacyTaskMedia{Type: legacyTaskMediaTypeVideo}
	}
	storeLegacyTaskMediaCache(key, media)
	return media
}

func probeLegacyTaskMedia(c *gin.Context, task *model.Task) (legacyTaskMedia, error) {
	rawURL := strings.TrimSpace(task.GetResultURL())
	if strings.HasPrefix(rawURL, "data:") {
		return legacyTaskMediaFromDataURL(rawURL), nil
	}
	if isTaskMediaFallbackLoop(rawURL, task.TaskID) {
		return legacyTaskMedia{}, errTaskMediaRequestRejected
	}
	descriptor := &relaychannel.TaskContentRequest{
		URL:            rawURL,
		Method:         http.MethodGet,
		Credentialless: true,
	}
	clientHeaders := map[string]string{
		"Range": fmt.Sprintf("bytes=0-%d", legacyTaskMediaSniffBytes-1),
	}
	resp, err := openTaskMedia(c, task, descriptor, clientHeaders, legacyTaskMediaProbeTimeout)
	if err != nil {
		return legacyTaskMedia{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return legacyTaskMedia{}, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	if media := legacyTaskMediaFromContentType(resp.Header.Get("Content-Type")); media.Type != "" {
		return media, nil
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, legacyTaskMediaSniffBytes))
	if err != nil && len(head) == 0 {
		return legacyTaskMedia{}, err
	}
	return legacyTaskMediaFromContentType(http.DetectContentType(head)), nil
}

// legacyTaskMediaFromContentType classifies an image/* or video/* media type.
// Generic or unknown types return an empty result so the caller can sniff.
func legacyTaskMediaFromContentType(contentType string) legacyTaskMedia {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || len(mediaType) > 255 {
		return legacyTaskMedia{}
	}
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return legacyTaskMedia{Type: legacyTaskMediaTypeImage, MimeType: mediaType}
	case strings.HasPrefix(mediaType, "video/"):
		return legacyTaskMedia{Type: legacyTaskMediaTypeVideo, MimeType: mediaType}
	default:
		return legacyTaskMedia{}
	}
}

func legacyTaskMediaFromDataURL(dataURL string) legacyTaskMedia {
	header, payload, found := strings.Cut(dataURL, ",")
	if !found {
		return legacyTaskMedia{}
	}
	mediaType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if media := legacyTaskMediaFromContentType(mediaType); media.Type != "" {
		return media
	}
	if !strings.HasSuffix(header, ";base64") {
		return legacyTaskMedia{}
	}
	// Decode just enough of the payload to sniff the leading bytes.
	encodedPrefix := payload
	if limit := base64.StdEncoding.EncodedLen(legacyTaskMediaSniffBytes); len(encodedPrefix) > limit {
		encodedPrefix = encodedPrefix[:limit]
	}
	head, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encodedPrefix)))
	if len(head) == 0 {
		return legacyTaskMedia{}
	}
	return legacyTaskMediaFromContentType(http.DetectContentType(head))
}
