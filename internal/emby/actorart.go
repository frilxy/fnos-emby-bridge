package emby

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fnos-emby-bridge/internal/fn"
)

// 飞牛影视不给「其他」类型库刮演员头像：person 接口里 profile 恒为空串
// （真机 /debug/person 实测），而这类库的演员图通常是刮削工具（MDC、JavSP 等）
// 按 Kodi/Emby 生态约定写进影片目录的 `.actors/演员名.jpg`。飞牛不读，Emby 客户端
// 又只是连桥接（虚拟库，不会扫盘），所以只能由桥接自己按文件系统兜底。

// actorArtDirs 本地演员图目录名（按优先级）：
//   - `.actors` / `.actor`：Kodi、Emby 4.8+、MDC/JavSP 的产出目录
//   - `actors`：部分工具不带前导点
var actorArtDirs = []string{".actors", ".actor", "actors"}

// actorArtExts 认可的图片扩展名（按优先级）。
var actorArtExts = []string{".jpg", ".jpeg", ".png", ".webp", ".tbn"}

// actorArtMaxWorks 单次查找最多看多少个作品目录：冷门演员可能有上百部作品，
// 每部都要一次 stream/list 才能拿到真实路径，不设上限会把一次头像请求拖成几十秒。
const actorArtMaxWorks = 12

// actorArtCacheTTL 演员图解析结果缓存时长（负结果也缓存，避免每次请求都扫盘）。
const actorArtCacheTTL = 10 * time.Minute

type actorArtEntry struct {
	path string // 命中的图片绝对路径；空串表示确认没有
	at   time.Time
}

var actorArtCache sync.Map // token|personGuid -> actorArtEntry

// normActorName 归一化演员名用于文件名比对：忽略大小写、空格（含全角）、
// 下划线、点与连字符——`神木 麗`、`神木_麗`、`神木麗` 视为同一个。
func normActorName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.NewReplacer(" ", "", "\u3000", "", "_", "", ".", "", "-", "", "·", "").Replace(s)
	return s
}

// matchActorArt 在 dir 下的演员图目录里找名字匹配的图片，返回文件路径。
func matchActorArt(dir string, names []string) string {
	if dir == "" || len(names) == 0 {
		return ""
	}
	for _, d := range actorArtDirs {
		base := filepath.Join(dir, d)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		// 先按原样精确匹配，再按归一化名字匹配（中文名一般一步就中）
		for _, e := range entries {
			if e.IsDir() || !hasActorArtExt(e.Name()) {
				continue
			}
			stem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			if stem == names[0] {
				return filepath.Join(base, e.Name())
			}
		}
		for _, e := range entries {
			if e.IsDir() || !hasActorArtExt(e.Name()) {
				continue
			}
			stem := normActorName(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
			for _, want := range names {
				if want != "" && stem == want {
					return filepath.Join(base, e.Name())
				}
			}
		}
	}
	return ""
}

func hasActorArtExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, want := range actorArtExts {
		if ext == want {
			return true
		}
	}
	return false
}

// actorNames 演员名候选（归一化后去重）：译名 + 原名。
func actorNames(name, originalName string) []string {
	out := make([]string, 0, 2)
	seen := map[string]struct{}{}
	for _, s := range []string{name, originalName} {
		if n := normActorName(s); n != "" {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

// localActorArt 解析某演员的本地头像文件路径（带缓存）。
// 返回空串表示没找到（不区分「没图」与「没权限」，诊断走 /debug/actor）。
func (h *Handler) localActorArt(ctx context.Context, personGuid, name, originalName string) string {
	key := h.fnOf(ctx).Token + "|" + personGuid
	if v, ok := actorArtCache.Load(key); ok {
		if e, ok := v.(actorArtEntry); ok && time.Since(e.at) < actorArtCacheTTL {
			return e.path
		}
	}
	path := h.findActorArt(ctx, personGuid, name, originalName)
	actorArtCache.Store(key, actorArtEntry{path: path, at: time.Now()})
	return path
}

// findActorArt 遍历演员作品目录找头像：作品路径来自 stream/list 的 files[].path
// （play_info 不带真实路径），作品目录与其上一级各找一次——.actors 可能放在
// 影片目录里（电影）或剧集/季目录里（剧）。
func (h *Handler) findActorArt(ctx context.Context, personGuid, name, originalName string) string {
	names := actorNames(name, originalName)
	if len(names) == 0 {
		return ""
	}
	client := h.fnOf(ctx)
	for i, w := range h.personWorks(ctx, personGuid) {
		if i >= actorArtMaxWorks {
			break
		}
		dir := filepath.Dir(h.mediaPathOf(ctx, client, w.Guid))
		if dir == "" || dir == "." {
			continue
		}
		if p := matchActorArt(dir, names); p != "" {
			return p
		}
		if p := matchActorArt(filepath.Dir(dir), names); p != "" {
			return p
		}
	}
	return ""
}

// mediaPathOf 取条目的真实文件路径（stream/list，30s 缓存由 h.streamList 负责）。
func (h *Handler) mediaPathOf(ctx context.Context, client *fn.Client, guid string) string {
	sl, err := h.streamList(ctx, client, guid)
	if err != nil || sl == nil {
		return ""
	}
	for _, f := range sl.Files {
		if f.Path != "" {
			return f.Path
		}
	}
	return ""
}

// serveLocalImage 回本地图片文件（演员图兜底）；读不到时退回 1x1 占位。
// 文件系统权限不足（桥接包用户没被授权访问该共享目录）也走占位，
// 具体原因用 /debug/actor 看。
func serveLocalImage(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		servePlaceholderPNG(w)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		servePlaceholderPNG(w)
		return
	}
	w.Header().Set("Content-Type", actorArtMime(path))
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// servePlaceholderPNG 头像占位（1x1 透明 PNG），与既有图片兜底一致。
func servePlaceholderPNG(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png1x1())
}

// actorArtMime 按扩展名给 Content-Type（未知一律 jpeg，客户端按图解码不挑）。
func actorArtMime(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".tbn":
		return "image/tbn"
	default:
		return "image/jpeg"
	}
}
