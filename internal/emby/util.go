package emby

import (
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// randUint32 32 位随机数（PlaySessionId 等需要每次唯一的场景）。
func randUint32() uint32 { return rand.Uint32() }

// rateLimiter 一个最小的令牌桶限速器，避免引入 x/time 依赖。
// Wait(ctx) 阻塞直到能获取一个令牌（rate 个/秒）。
type rateLimiter struct {
	mu     sync.Mutex
	tokens float64
	rate   float64 // 每秒补充速率
	last   time.Time
}

func newRateLimiter(perSec int) *rateLimiter {
	return &rateLimiter{tokens: float64(perSec), rate: float64(perSec), last: time.Now()}
}

// Wait 阻塞直到获取一个令牌。
func (l *rateLimiter) Wait(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.rate {
			l.tokens = l.rate
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			return
		}
		need := (1 - l.tokens) / l.rate
		sleep := time.Duration(need * float64(time.Second))
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		l.mu.Lock()
	}
}

// decodeJSON 从请求体解码 JSON。body 为空或解码失败时 dst 保持零值。
func decodeJSON(r *http.Request, dst interface{}) {
	if r.Body == nil {
		return
	}
	defer r.Body.Close()
	_ = json.NewDecoder(r.Body).Decode(dst)
}

// jsonEncode 把 v 编码到 w。
func jsonEncode(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// copyStream 把源流原样写到 dst，避免中间缓冲。
func copyStream(dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
}

// png1x1 一个 1x1 透明 PNG，用于 /Branding/Icon/{key}。
func png1x1() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2B, 0x3E, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
}

// langName ISO639 代码 → 英文全名（官方 DisplayLanguage 存全名，
// Yamby "显示语言: Japanese" 行直接渲染；未知代码返回空）。
var langNames = map[string]string{
	"jpn": "Japanese", "eng": "English", "chi": "Chinese", "zho": "Chinese",
	"kor": "Korean", "tha": "Thai", "fra": "French", "fre": "French",
	"deu": "German", "ger": "German", "spa": "Spanish", "rus": "Russian",
	"por": "Portuguese", "ita": "Italian", "ind": "Indonesian",
	"vie": "Vietnamese", "hin": "Hindi", "tam": "Tamil",
	"und": "", "zz-unknow": "",
}

func langName(code string) string {
	if code == "" {
		return ""
	}
	if full, ok := langNames[strings.ToLower(code)]; ok {
		return full
	}
	return ""
}

// parseFPS 解析飞牛帧率字符串 "23.98 fps" → 23.98（官方 RealFrameRate 是数值）。
func parseFPS(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "fps"))
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return f
}

// ---------------------------------------------------------------------------
// 以下为 fork 新增（上游 ssabv/fnos-emby-bridge 无这些函数）
// ---------------------------------------------------------------------------

// mimeByExt 容器扩展名 → MIME。
// 背景：上游 /v/api/v1/media/range 一律返回 application/octet-stream，
// 部分播放器（不依赖内容嗅探、只看 MIME 的实现）会因此拒绝播放。
var mimeByExt = map[string]string{
	"mp4":  "video/mp4",
	"m4v":  "video/mp4",
	"mkv":  "video/x-matroska",
	"webm": "video/webm",
	"mov":  "video/quicktime",
	"avi":  "video/x-msvideo",
	"ts":   "video/mp2t",
	"m2ts": "video/mp2t",
	"flv":  "video/x-flv",
	"wmv":  "video/x-ms-wmv",
	"mpg":  "video/mpeg",
	"mpeg": "video/mpeg",
	"m2v":  "video/mpeg",
	"rmvb": "application/vnd.rn-realmedia-vbr",
}

// mimeForExt 按扩展名（可带点，如 ".mkv"）返回 MIME；未知返回空串。
func mimeForExt(ext string) string {
	return mimeByExt[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))]
}

// genericMime 判断 Content-Type 是否属于「没有信息量」的通用类型
// （空、application/octet-stream 等）；这类值应当用扩展名推导的 MIME 覆盖。
func genericMime(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "", "application/octet-stream", "binary/octet-stream", "application/binary":
		return true
	}
	return false
}

// parseTotalSize 从 Content-Range "bytes 0-0/6721466674" 解析资源总长度。
// 解析失败返回 0（调用方应退化为不设置 Content-Length）。
func parseTotalSize(contentRange string) int64 {
	i := strings.LastIndexByte(contentRange, '/')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(contentRange[i+1:]), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// hostileAudioCodecs 移动端/电视端普遍无法直接解码的音频编码。
// DTS/杜比（TrueHD）需要授权，多数 Android 设备不带解码器；
// FLAC/ALAC/PCM 一般也只在桌面端或软解播放器可用。
// 说明：这里只用于「标题提示」，不参与任何播放决策。
var hostileAudioCodecs = map[string]bool{
	"dts": true, "dts-hd": true, "dtshd": true, "dts-hd ma": true, "dts:x": true,
	"truehd": true, "mlp": true, "flac": true, "alac": true,
	"pcm": true, "pcm_s16le": true, "pcm_s24le": true, "pcm_bluray": true,
}

// audioHintEnabled 音轨兼容性提示开关（默认开启，AUDIO_TRACK_HINT=0 关闭）。
func audioHintEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AUDIO_TRACK_HINT"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// audioDisplayTitle 在音轨显示标题后追加兼容性提示，方便用户手动挑一条能直解的轨道。
//
// 刻意不做「自动切到兼容音轨」：备选轨常常是导演评论轨（实测霸王别姬、
// 花样年华等文件即如此），自动切换会让用户听到错误音轨，风险高于收益。
func audioDisplayTitle(title, codec string) string {
	if !audioHintEnabled() {
		return title
	}
	if !hostileAudioCodecs[strings.ToLower(strings.TrimSpace(codec))] {
		return title
	}
	const tag = "需软解"
	if strings.Contains(title, tag) {
		return title
	}
	if title == "" {
		return tag
	}
	return title + " · " + tag
}
