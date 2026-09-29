package emby

import (
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
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
