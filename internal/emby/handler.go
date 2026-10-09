// Package emby 实现 Emby 官方客户端会访问的最小协议端点集，
// 内部把请求转成飞牛影视 /v/api/v1 私有接口调用。
//
// 客户端首连接顺序（Emby 官方 App / Web）：
//  1. GET /
//  2. GET /System/Info  +  /System/Info/Public
//  3. POST /Users/AuthenticateByName
//  4. GET /Branding/Configuration?api_key=...  (品牌/能力)
//  5. GET /Items?Recursive=true&IncludeItemTypes=Folder,Movie,Episode...
//  6. GET /Items/{id}  详情
//  7. POST /Videos/{id}/Playback  /  GET /Videos/{id}/Stream（媒体流）
//  8. POST /Items/{id}/Progress
package emby

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"fnos-emby-bridge/internal/fn"
)

// Handler 把 Emby 协议端点映射到飞牛接口。
// 0.9.8 真机契约：mediadb/list 直接列全部媒体库，无需 SEED_GUIDS 配置。
type Handler struct {
	FN *fn.Client
	// ServerName 在 /System/Info 里返回给客户端。
	ServerName string
	// ServerID 是 32 位 hex 的服务器唯一标识（GUID 形态，无连字符）。
	//
	// Emby/Jellyfin 的 ServerId 都是这种形态（飞牛原生面实测：
	// 9da9a321945b4e60a7488ea2be37cb9f）。此前固定返回字符串
	// "fnos-emby-bridge"，格式不合法的 ServerId 会让部分客户端（如小幻影视）
	// 无法正确登记服务器，表现为打不开媒体库或详情。
	ServerID string
	// Host 是客户端访问本桥接的主机:端口，用于拼绝对媒体 URL。
	Host string

	// 多用户会话池：Emby 客户端 token → 飞牛会话。
	// 此前全局共用一个飞牛 token，后登录者把先登录者的 token 顶掉，
	// 所有人进度/收藏/已看全写到最后登录的飞牛账号上（用户实测进度互串）。
	sessMu     sync.Mutex
	sessions   map[string]*embySession
	sessLoaded bool

	// 115 云盘限速器（1 req/s），复刻 fntv-electron 的 globalLimiter
	lim115 *rateLimiter

	// 云盘直连共享 http.Client（流式，Timeout=0）
	cloudHTTP *http.Client

	// 库自定义图解析（admin 后台配置；guid -> 图片 URL，空串=未自定义）
	libImage func(guid string) string
}

// embySession 一个客户端登录会话：持有该用户自己的飞牛 token。
// FNToken/UserName/PassHash 落盘 bridge-sessions.json（重启恢复会话，
// 客户端旧 token 继续有效，不触发重新登录）。client 仅存内存。
type embySession struct {
	client *fn.Client `json:"-"`

	FNToken  string `json:"fn_token"`
	UserName string `json:"username"`
	PassHash string `json:"pass_hash"` // SHA256 hex，token 失效静默重登用
	Created  int64  `json:"created"`
	// verifiedAt 最近一次确认 FNToken 有效的时间（unix 秒，仅内存）。
	// TTL 内重复登录（客户端频繁刷新/重连）直接复用，不再碰飞牛登录接口。
	verifiedAt int64
}

// sessionVerifyTTL 缓存会话免验证窗口：窗口内 AuthenticateByName 零飞牛请求
// （飞牛 loginByPassword 有频控，客户端每次刷新都真登录会触发限流）。
const sessionVerifyTTL = 10 * time.Minute

// sessionsFile 会话池落盘位置（二进制工作目录，与 bridge-progress.json 同目录）。
const sessionsFile = "bridge-sessions.json"

// SetLibImageResolver 注入库自定义图查询（main 装配 admin.Store）。
func (h *Handler) SetLibImageResolver(f func(guid string) string) { h.libImage = f }

// NewHandler 构造一个 Handler。
func NewHandler(fnClient *fn.Client, serverName, host string) *Handler {
	if serverName == "" {
		serverName = "fnos"
	}
	base := ""
	if fnClient != nil {
		base = fnClient.BaseURL
	}
	return &Handler{FN: fnClient, ServerName: serverName, Host: host,
		ServerID: serverID(base, serverName),
		lim115:   newRateLimiter(1), sessions: map[string]*embySession{}}
}

// serverID 生成稳定的 32 位 hex 服务器标识（GUID 形态）。
//
// 由飞牛地址 + 服务器名派生：同一套部署每次启动结果一致（客户端不会因为
// ServerId 变化而重新登记服务器），不同 NAS 之间又不会撞车。
// 不能用随机值——每次重启都换 ServerId 会让客户端丢失服务器关联。
func serverID(baseURL, serverName string) string {
	return fn.SHA256Hex("fnos-emby-bridge|" + baseURL + "|" + serverName)[:32]
}

// Routes 返回 http.ServeMux 的路由表，供 main 直接挂载。
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	// 客户端探测/握手
	mux.HandleFunc("GET /{$}", h.handleRoot)
	mux.HandleFunc("GET /System/Info", h.handleSystemInfo)
	mux.HandleFunc("GET /System/Info/Public", h.handleSystemInfoPublic)
	mux.HandleFunc("GET /Branding/Configuration", h.handleBranding)
	mux.HandleFunc("GET /Branding/Icon/{key}", h.handleBrandingIcon)
	mux.HandleFunc("POST /Users/AuthenticateByName", h.handleAuthenticate)
	mux.HandleFunc("GET /Users/Me", h.handleUsersMe)
	mux.HandleFunc("GET /Users/{uid}", h.handleUsersByID)
	// /Users/Public 官方返回纯 UserDto 数组（web 客户端 getPublicUsers 直接
	// users.slice(users)，给 QueryResult 对象会崩）
	mux.HandleFunc("GET /Users/Public", h.handleUsersPublic)

	// 媒体库
	mux.HandleFunc("GET /Items", h.handleItems)
	mux.HandleFunc("GET /Users/{uid}/Items", h.handleItems)
	mux.HandleFunc("GET /Users/{uid}/Views", h.handleViews)

	// fork 新增：飞牛原生 Jellyfin 面（8005）实现、而桥接此前缺失/形状不符的端点。
	// 这些是客户端"打开媒体库/详情"的必经路径，缺了会直接失败：
	//
	//   /UserViews                 列媒体库的另一个官方路径。此前落到兜底返回
	//                              空列表 → 客户端看到的媒体库列表是空的。
	//   /Items/Filters             此前被 /Items/{id} 通配吃掉，拿 "Filters" 当
	//   /Users/{uid}/Items/Filters 影片 ID 去查飞牛 → 404，客户端开库时调用它，
	//                              404 会让媒体库页打不开。（字面量段优先于通配段，
	//                              显式注册即可抢回）
	//   /Items/Counts              此前返回 QueryResult 对象，而 Emby 的 ItemCounts
	//   /Users/{uid}/Items/Counts  是另一套字段的对象。
	//   /Library/VirtualFolders    Emby 返回数组，不是 QueryResult。
	//   /Plugins                    Emby 返回 PluginInfo[] 数组。
	//   /Users/{uid}/GroupingOptions 返回数组。
	mux.HandleFunc("GET /UserViews", h.handleViews)
	mux.HandleFunc("GET /Items/Filters", h.handleQueryFilters)
	mux.HandleFunc("GET /Users/{uid}/Items/Filters", h.handleQueryFilters)
	mux.HandleFunc("GET /Items/Counts", h.handleItemCountsAll)
	mux.HandleFunc("GET /Users/{uid}/Items/Counts", h.handleItemCountsAll)
	mux.HandleFunc("GET /Library/VirtualFolders", h.handleVirtualFolders)
	mux.HandleFunc("GET /Plugins", h.handleEmptyArray)
	mux.HandleFunc("GET /Users/{uid}/GroupingOptions", h.handleEmptyArray)

	mux.HandleFunc("GET /Items/{id}", h.handleItemByID)
	// 详情页（UserLibraryService 标准路径）：Yamby 点开剧/季/集详情走
	// /Users/{uid}/Items/{id}，未注册会落兜底返回空列表 → 详情页空白
	mux.HandleFunc("GET /Users/{uid}/Items/{id}", h.handleItemByID)

	// 播放（PlaybackInfo 是官方客户端播放入口；Playback 为老端点）
	mux.HandleFunc("POST /Items/{id}/PlaybackInfo", h.handlePlaybackInfo)
	mux.HandleFunc("GET /Items/{id}/PlaybackInfo", h.handlePlaybackInfo)
	mux.HandleFunc("POST /Videos/{id}/Playback", h.handlePlayback)
	mux.HandleFunc("GET /Videos/{id}/Playback", h.handlePlayback)
	// Emby apiclient 用小写 stream，Go ServeMux 大小写敏感，两个都注册。
	// 官方 web 客户端直连时请求 /Videos/{id}/stream.mp4,mkv,mov,webm,avi,ts
	// （容器后缀拼在段里），单段通配兜住所有 stream.* 变体。
	mux.HandleFunc("GET /Videos/{id}/stream", h.handleStream)
	mux.HandleFunc("GET /Videos/{id}/Stream", h.handleStream)
	mux.HandleFunc("GET /Videos/{id}/{rest}", h.handleVideosSub)

	// 进度回传（官方客户端走 /Sessions/Playing/*，老客户端走 /Items/{id}/Progress）
	mux.HandleFunc("POST /Sessions/Playing", h.handleSessionsPlaying)
	mux.HandleFunc("POST /Sessions/Playing/Progress", h.handleSessionsPlaying)
	mux.HandleFunc("POST /Sessions/Playing/Stopped", h.handleSessionsStopped)
	mux.HandleFunc("POST /Items/{id}/Progress", h.handleProgress)

	// 剧集导航（官方 App 点开剧走 /Shows/* 而非 /Items?ParentId=）
	mux.HandleFunc("GET /Shows/{id}/Seasons", h.handleShowsSeasons)
	mux.HandleFunc("GET /Shows/{id}/Episodes", h.handleShowsEpisodes)
	// 首页推荐位（官方 App 首页会请求，缺 404 会弹错）
	mux.HandleFunc("GET /Shows/NextUp", h.handleNextUp)
	mux.HandleFunc("GET /Items/Resume", h.handleResume)
	mux.HandleFunc("GET /Users/{uid}/Items/Resume", h.handleResume)
	mux.HandleFunc("GET /Users/{uid}/Items/Latest", h.handleLatest)
	// 首页布局偏好（GET 读取 / POST 保存）
	mux.HandleFunc("GET /DisplayPreferences/{key}", h.handleDisplayPrefsGet)
	mux.HandleFunc("POST /DisplayPreferences/{key}", h.handleDisplayPrefsPost)

	// WebSocket（官方 App 登录后必连 /embysocket，缺 404 会导致"无法连接"）
	mux.HandleFunc("GET /embysocket", h.handleEmbySocket)
	// 官方 web 客户端/Emby Theater 用标准路径 /embywebsocket（wss://host/embywebsocket）
	mux.HandleFunc("GET /embywebsocket", h.handleEmbySocket)

	// 海报/横图：Emby 协议客户端会请求任意 ImageType（Primary/Logo/Thumb/
	// Backdrop/Banner/Art...，参照 Emby-In-One 透传集合），只注册 Primary 的话
	// Logo 等落兜底变 1x1 透明图 → "剧的 logo 没有"
	mux.HandleFunc("GET /Items/{id}/Images/{imgType}", h.handleItemImage)
	mux.HandleFunc("GET /Items/{id}/Images/{imgType}/{imgIndex}", h.handleItemImage)
	// 播放前片头询问（返回空列表，避免客户端异常）
	mux.HandleFunc("GET /Items/{id}/Intros", h.handleEmptyList)
	// /Sessions 官方返回纯数组（不是 QueryResult）——Yamby Kotlin 反序列化遇到
	// 对象当数组直接抛异常，登录后卡死不再发任何请求
	mux.HandleFunc("GET /Sessions", h.handleEmptyArray)
	// 会话能力声明（Yamby 登录后会拉，控制可用命令集）
	mux.HandleFunc("GET /Sessions/Capabilities/Full", h.handleSessionCapabilities)
	mux.HandleFunc("POST /Sessions/Capabilities/Full", h.handleSessionCapabilitiesPost)
	// Yamby 登录后初始化拉的其余端点（dex 逆向确认）：形态错=Kotlin 反序列化崩
	// System/Configuration 必须是配置对象（fallback 的 QueryResult 缺非空字段 → MissingFieldException）
	mux.HandleFunc("GET /System/Configuration", h.handleSystemConfiguration)
	// ScheduledTasks 官方返回 TaskInfo[] 纯数组（服务器管理页；fallback 的对象形态会崩）
	mux.HandleFunc("GET /ScheduledTasks", h.handleEmptyArray)
	mux.HandleFunc("GET /ScheduledTasks/Running/{id}", h.handleEmptyArray)
	// Yamby 媒体库筛选维度（Genres/Persons/Tags/Years/OfficialRatings）与 Upcoming：
	// QueryResult 列表语义，fallback 形态已对；显式注册避免每次命中兜底日志
	mux.HandleFunc("GET /Genres", h.handleEmptyList)
	mux.HandleFunc("GET /Persons", h.handleEmptyList)
	mux.HandleFunc("GET /Tags", h.handleEmptyList)
	mux.HandleFunc("GET /Years", h.handleEmptyList)
	mux.HandleFunc("GET /OfficialRatings", h.handleEmptyList)
	mux.HandleFunc("GET /Shows/Upcoming", h.handleEmptyList)
	mux.HandleFunc("GET /LiveTv/Channels", h.handleEmptyList)
	// 收藏（详情页❤️，真机抓包飞牛 PUT/DELETE item/favorite）：
	// Emby 标准路径 POST/DELETE /Users/{uid}/FavoriteItems/{id}
	mux.HandleFunc("POST /Users/{uid}/FavoriteItems/{id}", h.handleFavorite)
	mux.HandleFunc("DELETE /Users/{uid}/FavoriteItems/{id}", h.handleFavorite)
	// 元数据刷新（详情页下拉刷新 → 飞牛 item/refresh）
	mux.HandleFunc("POST /Items/{id}/Refresh", h.handleRefresh)
	// 主题音乐/主题视频：web 客户端详情页必调，读 ThemeVideosResult.Items
	mux.HandleFunc("GET /Items/{id}/ThemeMedia", h.handleThemeMedia)
	// 相似推荐（剧/电影详情页"更多类似"区块）：飞牛无对应数据，返回空列表
	mux.HandleFunc("GET /Items/{id}/Similar", h.handleEmptyList)
	mux.HandleFunc("GET /Items/{id}/Counts", h.handleItemCounts)
	// 外挂字幕下载（Emby-In-One 确认路径 /Videos/{itemId}/{msId}/Subtitles/{index}/Stream.{fmt}）：
	// 飞牛无字幕内容接口，内嵌字幕由客户端直连容器读取；此处注册避免兜底 JSON 被当字幕解析
	mux.HandleFunc("GET /Videos/{id}/{msId}/Subtitles/{index}/{rest}", h.handleSubtitleStream)
	// 媒体统计（客户端设置页/仪表盘显示）——已在上方媒体库块改为 handleItemCountsAll
	// 特别收录：web 客户端 getSpecialFeatures 直接 items.slice(...)，必须纯数组
	mux.HandleFunc("GET /Users/{uid}/Items/{id}/SpecialFeatures", h.handleEmptyArray)
	mux.HandleFunc("GET /Items/{id}/SpecialFeatures", h.handleEmptyArray)

	// 兜底：未显式实现的端点一律返回"空成功"（Emby 形态），
	// 官方客户端对任何 404 都可能弹错；命中兜底会打日志，便于按需补精确实现。
	mux.HandleFunc("/", h.handleFallback)

	return mux
}

// handleFallback 兜底未注册端点。ServeMux 优先命中更具体的已注册模式，这里只接漏网的。
func (h *Handler) handleFallback(w http.ResponseWriter, r *http.Request) {
	log.Printf("[fallback] %s %s", r.Method, r.URL.Path)
	// 图片类路径：回 1x1 透明 PNG
	if strings.Contains(r.URL.Path, "/Images/") {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png1x1())
		return
	}
	// 已看标记：POST /Users/{uid}/PlayedItems/{id}（看完）→ 飞牛 item/watched + 进度写满；
	// DELETE（取消已看）→ 清进度。真机抓包：飞牛播放器播完调 item/watched 同效果
	if m := playedItemsRe.FindStringSubmatch(r.URL.Path); m != nil {
		id := m[1]
		ctx := r.Context()
		if r.Method == http.MethodPost {
			if info, err := h.fnOf(ctx).PlayInfoByGuid(ctx, id); err == nil {
				_ = h.fnOf(ctx).RecordProgress(ctx, id, info.MediaGuid, playInfoSeconds(info), playInfoSeconds(info))
			}
			_ = h.fnOf(ctx).WatchItem(ctx, id)
			touchProgress(id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodDelete {
			// 取消已看：飞牛 DELETE item/watched（APK 逆向实测）+ 清进度
			if err := h.fnOf(ctx).UnwatchItem(ctx, id); err != nil {
				log.Printf("[watched] 取消已看 %s: %v", id, err)
			}
			if info, err := h.fnOf(ctx).PlayInfoByGuid(ctx, id); err == nil {
				_ = h.fnOf(ctx).RecordProgress(ctx, id, info.MediaGuid, 0, playInfoSeconds(info))
			}
			resumeMu.Lock()
			resumeAt = time.Time{}
			resumeMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	// 写操作：静默成功
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// 其余返回空列表形态（首页各区块均为列表语义）
	writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
}

// playedItemsRe 匹配 /Users/{uid}/PlayedItems/{id}（大小写归一发生在路由层之前）。
var playedItemsRe = regexp.MustCompile(`(?i)^/Users/[^/]+/PlayedItems/([0-9a-fA-F]+)$`)

// Handler 返回包了 /emby 前缀剥离 + 大小写归一 + 请求日志的完整 handler。
// Emby 官方客户端（Android/iOS/TV/Web）所有请求都带 /emby 前缀，必须兼容。
func (h *Handler) Handler() http.Handler {
	// withSession 按请求 token 选定飞牛会话注入 ctx——多用户进度/收藏隔离的入口
	// repairMangledURLPath 紧随日志之后、最先改写路径：先把 "/embyhttp:/host/..."
	// 还原成 "/Videos/..."，后续大小写归一与 /emby 剥离才能作用于真实路径。
	return logRequests(repairMangledURLPath(normalizePathCase(stripEmbyPrefix(h.withSession(h.Routes())))))
}

// routeSegmentCase 已知路由静态段 小写→规范大小写（与 Routes() 注册一致）。
// 真 Emby Server（ASP.NET）路由不区分大小写，官方 web 客户端全程小写调用
// （/emby/system/info/public 等），Go ServeMux 大小写敏感，必须归一。
var routeSegmentCase = map[string]string{
	"system": "System", "info": "Info", "public": "Public",
	"users": "Users", "authenticatebyname": "AuthenticateByName", "me": "Me",
	"views": "Views", "items": "Items", "latest": "Latest", "resume": "Resume",
	"shows": "Shows", "seasons": "Seasons", "episodes": "Episodes", "nextup": "NextUp",
	"videos": "Videos", "stream": "Stream", "playbackinfo": "PlaybackInfo",
	"playback": "Playback", "progress": "Progress",
	"sessions": "Sessions", "playing": "Playing", "stopped": "Stopped",
	"branding": "Branding", "configuration": "Configuration", "icon": "Icon",
	"displaypreferences": "DisplayPreferences", "intros": "Intros",
	"images": "Images", "primary": "Primary", "thememedia": "ThemeMedia",
	"themesongs": "ThemeSongs", "themevideos": "ThemeVideos",
	// fork 新增（与上面新增的路由配套，否则客户端发小写路径会落到兜底）
	"userviews": "UserViews", "filters": "Filters", "plugins": "Plugins",
	"groupingoptions": "GroupingOptions", "virtualfolders": "VirtualFolders",
	"library": "Library", "counts": "Counts",
}

// normalizePathCase 把路径中命中已知路由段的段重写为规范大小写，
// 并在应用层清洗重复斜杠（//emby→/emby）——Go mux 遇到重复斜杠会 301，
// OkHttp 客户端跟 301 时 POST 会被降级成 GET（Yamby/AfuseKt 登录即失败，
// go-emby 为此专门做过 AfuseKt 回归），所以必须在 301 之前就地修正。
// guid/未知段（如 32 位 hex）原样保留；query 不受影响。
func normalizePathCase(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "//") {
			r.URL.Path = collapseSlashes(r.URL.Path)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
		}
		segs := strings.Split(r.URL.Path, "/")
		for i, s := range segs {
			if canon, ok := routeSegmentCase[strings.ToLower(s)]; ok {
				segs[i] = canon
			}
		}
		r.URL.Path = strings.Join(segs, "/")
		if r.URL.Path != "/" {
			r.URL.Path = strings.TrimRight(r.URL.Path, "/")
		}
		next.ServeHTTP(w, r)
	})
}

// collapseSlashes 把连续多个 '/' 折叠为一个。
func collapseSlashes(p string) string {
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// logRequests 全量访问日志（排障期）。stream 大流量长连接结束时打一行，不影响吞吐。
// api_key/token 值打码，避免日志落凭据。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		// fork: 进站路径快照。内部中间件会重写 r.URL.Path（/emby 剥离、大小写归一、
		// 畸形绝对 URL 修复），日志必须记录客户端真正发来的路径，
		// 否则这类"路径拼接错"的问题在日志里会被抹掉、无从排查。
		origPath := r.URL.Path
		next.ServeHTTP(rw, r)
		q := r.URL.RawQuery
		if i := strings.Index(q, "api_key="); i >= 0 {
			if j := strings.IndexByte(q[i:], '&'); j >= 0 {
				q = q[:i] + "api_key=***&" + q[i+j+1:]
			} else {
				q = q[:i] + "api_key=***"
			}
		}
		if len(q) > 400 {
			q = q[:400] + "..."
		}
		// 排障期顺带记录客户端标识（X-Emby-Authorization 里 Client/Device）
		cl := r.Header.Get("X-Emby-Authorization")
		if cl != "" {
			if i := strings.IndexByte(cl, ','); i > 0 {
				cl = cl[:i]
			}
		}
		log.Printf("[http] %d %s %s?%s (%s) [%s]", rw.status, r.Method, origPath, q, time.Since(start).Round(time.Millisecond), cl)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Hijack 透传给底层 writer：WebSocket 升级（gorilla）需要 http.Hijacker，
// 包装层不透传的话 Upgrade 返回 500。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return hj.Hijack()
}

// Flush 透传（流式响应需要）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// stripEmbyPrefix 剥掉 "/emby" 路径前缀，让带前缀的官方客户端请求命中同一套路由。
// 循环剥离：Yamby 等客户端的 API 路径自带 /emby 前缀，若服务器地址 Path
// 里也填了 /emby，拼接会得到 /emby/emby/...（go-emby 同款处理为 AfuseKt 做过回归）。
func stripEmbyPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			switch {
			case r.URL.Path == "/emby" || r.URL.Path == "/emby/":
				r.URL.Path = "/"
			case strings.HasPrefix(strings.ToLower(r.URL.Path), "/emby/"):
				r.URL.Path = strings.TrimPrefix(r.URL.Path, "/emby")
			default:
				next.ServeHTTP(w, r)
				return
			}
		}
	})
}

// repairMangledURLPath 修复「客户端把绝对播放地址拼到自己 base 后面」产生的畸形路径。
//
// 实测（Yamby + 本桥接）：
//
//	桥接返回 MediaSource.Path / DirectStreamUrl =
//	    http://192.168.10.229:8096/Videos/{id}/stream?MediaSourceId=...&Static=true
//	客户端基于自身 base（http://192.168.10.229:8096/emby）做字符串拼接，得到
//	    http://192.168.10.229:8096/embyhttp://192.168.10.229:8096/Videos/{id}/stream?...
//	Go 解析请求行时把 "//" 归一为 "/"，服务端实际收到：
//	    /embyhttp:/192.168.10.229:8096/Videos/{id}/stream?...
//
// 这种路径不匹配任何已注册路由，会落到兜底 handler —— 兜底对 GET 返回
// 200 + 空 QueryResult JSON，于是播放器拿到的是 49 字节 JSON 而不是视频字节，
// 而且因为状态码是 200，客户端连报错都没有，表现为「点开视频无法播放」。
//
// 修复方式是在入站侧把 scheme+host 摘掉，还原成服务器内真实路径，
// 让请求正常命中 handleStream。
//
// 为什么不在出站侧改成相对路径？因为 handlePlayback 处有明确注释：
// 「Path 必须真实 URL：客户端直接交给播放器内核（fn:// 伪协议零请求失败）」——
// 存在一类客户端会把 Path 原样交给播放器，改成相对路径会把它弄坏。
// 入站修复可以让两类客户端同时工作。
func repairMangledURLPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := stripAbsoluteURLPrefix(r.URL.Path); ok {
			log.Printf("[repair] 路径含绝对 URL 前缀，已还原: %s → %s", r.URL.Path, p)
			r.URL.Path = p
		}
		next.ServeHTTP(w, r)
	})
}

// stripAbsoluteURLPrefix 从形如 "/embyhttp:/host/xxx"、"/embyhttp://host/xxx"
// 的畸形路径里剥掉 scheme+host，返回 "/xxx"。第二个返回值表示是否发生改写。
// 注意 Go 会把请求行里的 "http://" 归一成 "http:/"，两种形态都要认。
func stripAbsoluteURLPrefix(p string) (string, bool) {
	lower := strings.ToLower(p)
	idx := -1
	for _, s := range []string{"http://", "https://", "http:/", "https:/"} {
		if i := strings.Index(lower, s); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	if idx < 0 {
		return p, false
	}
	rest := lower[idx:]
	skip := len("http:/")
	switch {
	case strings.HasPrefix(rest, "https://"):
		skip = len("https://")
	case strings.HasPrefix(rest, "http://"):
		skip = len("http://")
	case strings.HasPrefix(rest, "https:/"):
		skip = len("https:/")
	}
	after := p[idx+skip:]
	// 跳过 host[:port]，取第一个 '/' 之后的部分作为真实路径
	slash := strings.IndexByte(after, '/')
	if slash < 0 {
		return p, false
	}
	out := after[slash:]
	if len(out) <= 1 {
		return p, false
	}
	return out, true
}

// ---- 工具：从 query/header 取 api_key ----

func (h *Handler) apiKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		return strings.TrimPrefix(v, "Token ")
	}
	if v := r.Header.Get("X-Emby-Token"); v != "" {
		return v
	}
	return r.URL.Query().Get("api_key")
}

// validClient 校验请求是否带了 api_key（Emby 客户端总是带）。
func (h *Handler) validClient(r *http.Request) bool {
	return h.apiKey(r) != ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// ---- 端点实现 ----

func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"LocalAddress": hostOnly(r.Host),
		"ServerId":     h.ServerID,
		"ServerName":   h.ServerName,
		"Version":      "4.9.0.0",
		"ProductName":  "FNOS-EFBy-Bridge",
		"Id":           h.ServerID,
	})
}

func (h *Handler) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ServerName":                   h.ServerName,
		"Version":                      "4.9.0.0",
		"ProductName":                  "FNOS-EFBy-Bridge",
		"Id":                           h.ServerID,
		"IsAdminUser":                  true,
		"OperatingSystem":              "fnOS",
		"CanEnableAutoSignIn":          true,
		"SupportsAutoSignOutScheduler": true,
		"LocalAddress":                 hostOnly(r.Host),
		"StartupWizardCompleted":       true,
	})
}

// handleSystemInfoPublic 实现 GET /System/Info/Public。
//
// 字段对齐 Emby/Jellyfin 的 PublicSystemInfo（对照飞牛原生面 8005 实测响应——
// 那是飞牛自己实现给 Infuse/VidHub 用的，可作为权威参照）：
//
//	{"LocalAddress","ServerName","Version","ProductName","OperatingSystem",
//	 "Id","StartupWizardCompleted"}
//
// fork 修复：此前缺 LocalAddress / OperatingSystem / StartupWizardCompleted，
// 且 Id 固定为字符串 "fnos-emby-bridge" 而非 32 位 hex GUID。严格一些的客户端
// （如小幻影视）会校验这些字段，导致登记服务器失败、打不开媒体库或详情。
// 另：CanEnableAutoSignIn 属于需要鉴权的 SystemInfo，不属于 Public，已移出。
func (h *Handler) handleSystemInfoPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"LocalAddress":           hostOnly(r.Host),
		"ServerName":             h.ServerName,
		"Version":                "4.9.0.0",
		"ProductName":            "FNOS-EFBy-Bridge",
		"OperatingSystem":        "fnOS",
		"Id":                     h.ServerID,
		"StartupWizardCompleted": true,
	})
}

// hostOnly 去掉 "host:port" 里的端口（Emby 的 LocalAddress 是纯地址）。
func hostOnly(h string) string {
	if i := strings.LastIndexByte(h, ':'); i > 0 {
		return h[:i]
	}
	return h
}

func (h *Handler) handleBranding(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ApplicationName":      "FNOS-EFBy",
		"ApplicationVersion":   "4.9.0.0",
		"BaseThemeColor":       "#000000",
		"LogoUrl":              "",
		"SplashScreenImageUrl": "",
	})
}

func (h *Handler) handleBrandingIcon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png1x1())
}

// handleFavorite 收藏/取消收藏：POST/DELETE /Users/{uid}/FavoriteItems/{id}
// → 飞牛 PUT/DELETE item/favorite（真机抓包 body 只有 item_guid）。
// 返回最小 UserItem（客户端读 UserData.IsFavorite 更新 UI）。
func (h *Handler) handleFavorite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	on := r.Method == http.MethodPost
	if err := h.fnOf(r.Context()).SetFavorite(r.Context(), id, on); err != nil {
		log.Printf("[favorite] %s %s: %v", r.Method, id, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Id": id,
		"UserData": map[string]any{
			"IsFavorite": on,
			"Likes":      on,
		},
	})
}

// handleRefresh 元数据刷新：POST /Items/{id}/Refresh → 飞牛 item/refresh。
func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.fnOf(r.Context()).RefreshItem(r.Context(), id); err != nil {
		log.Printf("[refresh] %s: %v", id, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// personToEmby 把飞牛演员详情转成 Emby 人物 item（人物页）。
// 头像复用 base64 tag 机制（ImageTags.Primary = base64url(profile)）。
func personToEmby(pd *fn.PersonDetail) map[string]any {
	out := map[string]any{
		"Id":                      pd.Guid,
		"Name":                    pd.Name,
		"OriginalName":            pd.OriginalName,
		"Overview":                pd.Biography,
		"Type":                    "Person",
		"IsFolder":                false,
		"PrimaryImageAspectRatio": 0.6666666666666666,
		"UserData": map[string]any{
			"IsFavorite": pd.IsFavorite != 0,
		},
	}
	if pd.Profile != "" {
		out["ImageTags"] = map[string]any{"Primary": base64.URLEncoding.EncodeToString([]byte(pd.Profile))}
	}
	return out
}

// handleSystemConfiguration 实现 GET /System/Configuration。
// Yamby 登录后初始化拉取（dex 逆向确认）：必须是配置对象本身，
// 客户端 Kotlin 数据类的非空字段缺一个就 MissingFieldException——
// 走 fallback 的 QueryResult 形态会初始化协程崩死（登录后卡死候选雷之一）。
func (h *Handler) handleSystemConfiguration(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":                               "systemconfig",
		"IsStartupWizardCompleted":         true,
		"ServerName":                       "fnos",
		"EnableUPnP":                       false,
		"EnableRemoteAccess":               true,
		"EnableAutomaticRestart":           false,
		"EnableCaseSensitiveItemIds":       false,
		"EnableFolderView":                 false,
		"EnableGroupingIntoCollections":    false,
		"DisplaySpecialsWithinSeasons":     true,
		"EnableMetrics":                    false,
		"PreferredMetadataLanguage":        "zh-CN",
		"MetadataCountryCode":              "CN",
		"RemoteClientBitrateLimit":         0,
		"IdleSessionTimeout":               0,
		"LoginDisclaimer":                  "",
		"SplashscreenEnabled":              false,
		"SkipDeserializationForBasicTypes": false,
		"PluginRepositories":               []any{},
		"DisabledSubtitlesFetchers":        []any{},
	})
}

// userConfiguration 官方 web 客户端会读的 User.Configuration 字段（数组必须存在）。
func userConfiguration() map[string]any {
	return map[string]any{
		"LatestItemsExcludes": []any{},
		"MyMediaExcludes":     []any{},
		"GroupedFolders":      []any{},
		"OrderedViews":        []any{},
		"ProfilePin":          "",
	}
}

// ---- 多用户会话池 ----
//
// 每个客户端登录（AuthenticateByName）分配一个独立会话，持有该用户自己的
// 飞牛 token；此后所有请求按 api_key/X-Emby-Token 路由到对应会话，
// 进度/收藏/已看天然按飞牛账号隔离。
//
// 登录节流：同账号+密码的重复登录（客户端频繁刷新/重连）在 TTL 窗口内
// 直接复用缓存的飞牛 token，不调 loginByPassword——治「每次刷新算一次登录」
// 的飞牛限流。会话落盘重启恢复，桥接重启也不触发全员重登。

func (h *Handler) loadSessionsLocked() {
	if h.sessLoaded {
		return
	}
	h.sessLoaded = true
	b, err := os.ReadFile(sessionsFile)
	if err != nil {
		return
	}
	var saved map[string]*embySession
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for tk, s := range saved {
		if s == nil || s.FNToken == "" {
			continue
		}
		s.client = h.FN.Clone()
		s.client.SetToken(s.FNToken)
		h.sessions[tk] = s
	}
}

func (h *Handler) saveSessionsLocked() {
	b, err := json.Marshal(h.sessions)
	if err == nil {
		_ = os.WriteFile(sessionsFile, b, 0o600)
	}
}

// newEmbyToken 生成 64 位 hex 客户端访问令牌。
func newEmbyToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// acquireSession 取（或建）username+passHash 对应的会话，返回 Emby 客户端 token。
// TTL 内的重复登录纯缓存命中；超窗用轻接口验证 token 仍有效；失效才真重登。
func (h *Handler) acquireSession(ctx context.Context, userName, passHash string) (string, error) {
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	h.loadSessionsLocked()

	// 1. 缓存命中：同账号同密码
	for tk, s := range h.sessions {
		if s.UserName != userName || s.PassHash != passHash {
			continue
		}
		if time.Since(time.Unix(s.verifiedAt, 0)) < sessionVerifyTTL {
			return tk, nil // 窗口内：零飞牛请求
		}
		// 超窗：轻接口验证 token（mediadb/list，无副作用；走登录限流以外的通道）
		if _, err := s.client.MediaDBList(ctx); err == nil {
			s.verifiedAt = time.Now().Unix()
			h.saveSessionsLocked()
			return tk, nil
		}
		// token 失效：静默重登（复用会话条目，客户端 token 不变）
		c := h.FN.Clone()
		if err := c.Login(ctx, userName, passHash); err != nil {
			return "", err
		}
		s.client = c
		s.FNToken = c.Token
		s.verifiedAt = time.Now().Unix()
		h.saveSessionsLocked()
		log.Printf("[session] %s token 失效已静默重登（客户端 token 不变）", userName)
		return tk, nil
	}

	// 2. 新会话：唯一真正调飞牛登录的地方（用 clone，不污染全局 client）
	c := h.FN.Clone()
	if err := c.Login(ctx, userName, passHash); err != nil {
		return "", err
	}
	tk := newEmbyToken()
	s := &embySession{client: c, FNToken: c.Token, UserName: userName,
		PassHash: passHash, Created: time.Now().Unix(), verifiedAt: time.Now().Unix()}
	// OnAuthFail：运行中 token 失效 → 静默重登刷新（60s 防抖，防限流风暴）
	var lastReauth int64
	c.OnAuthFail = func() {
		h.sessMu.Lock()
		defer h.sessMu.Unlock()
		if time.Now().Unix()-lastReauth < 60 {
			return
		}
		lastReauth = time.Now().Unix()
		rc := h.FN.Clone()
		if err := rc.Login(context.Background(), userName, passHash); err != nil {
			log.Printf("[session] %s 静默重登失败: %v", userName, err)
			return
		}
		s.client = rc
		s.FNToken = rc.Token
		h.saveSessionsLocked()
		log.Printf("[session] %s 运行中 token 失效已自动重登", userName)
	}
	h.sessions[tk] = s
	h.saveSessionsLocked()
	log.Printf("[session] 新会话 %s（飞牛 token=%s...）", userName, c.Token[:min(8, len(c.Token))])
	return tk, nil
}

// clientOf 返回该请求应使用的飞牛客户端：登录会话命中用自己的，
// 否则（无 token / 未知 token / 内部调用）回落全局 admin 客户端。
func (h *Handler) clientOf(r *http.Request) *fn.Client {
	tk := h.apiKey(r)
	if tk == "" {
		return h.FN
	}
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	h.loadSessionsLocked()
	if s, ok := h.sessions[tk]; ok {
		s.client.BaseURL = h.FN.BaseURL // admin 热改地址时所有会话跟随
		return s.client
	}
	return h.FN
}

// ctxKeyFN context 键：请求级飞牛客户端（withSession 中间件注入）。
type ctxKeyFN struct{}

// withFN 把请求对应的飞牛客户端放进 context，供深层辅助函数取用
// （辅助函数普遍只收 ctx，逐层加 r 参数侵入太大）。
func withFN(ctx context.Context, c *fn.Client) context.Context {
	return context.WithValue(ctx, ctxKeyFN{}, c)
}

// fnOf 取 ctx 中的请求级飞牛客户端；未注入（内部调用/测试）回落全局 admin 客户端。
func (h *Handler) fnOf(ctx context.Context) *fn.Client {
	if c, ok := ctx.Value(ctxKeyFN{}).(*fn.Client); ok && c != nil {
		return c
	}
	return h.FN
}

// withSession 会话路由中间件：按请求 api_key/X-Emby-Token 选定飞牛会话并注入 ctx。
func (h *Handler) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(withFN(r.Context(), h.clientOf(r))))
	})
}

// handleAuthenticate 是 Emby 客户端"登录"的核心。
// 客户端 POST /Users/AuthenticateByName { UserName, Pwd/Pw }
// 兼容两种密码形态（真机验证：Yamby 发的 Pwd 已是 SHA256 hex，直接透传给飞牛；
// 明文密码则在此做 SHA256）。飞牛 0.9.8 登录要求 SHA256 hex。
func (h *Handler) handleAuthenticate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserName string `json:"UserName"`
		Pwd      string `json:"Pwd"`
		Pw       string `json:"Pw"`
	}
	// 官方 web 客户端发 form-encoded（Username=admin&Pw=123456，真机抓包确认），
	// Yamby 等移动端发 JSON——两种都支持
	if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		_ = r.ParseForm()
		req.UserName = r.PostFormValue("Username")
		if req.UserName == "" {
			req.UserName = r.PostFormValue("UserName")
		}
		req.Pw = r.PostFormValue("Pw")
		req.Pwd = r.PostFormValue("Password")
	} else {
		decodeJSON(r, &req)
	}

	// Pw（Emby 协议明文字段）优先；Pwd 为 64 位 hex 视为客户端已做 SHA256，直接透传
	pass := req.Pw
	if pass == "" {
		pass = req.Pwd
	}
	fnPass := pass
	if !fn.IsSHA256Hex(pass) {
		fnPass = fn.SHA256Hex(pass)
	}

	// 会话池：同账号+密码 TTL 内复用（不重复调飞牛登录，治限流）；
	// 新登录分配独立飞牛会话（进度/收藏按账号隔离，不顶号）
	embyToken, err := h.acquireSession(r.Context(), req.UserName, fnPass)
	if err != nil {
		log.Printf("[auth] 用户 %q 登录失败: %v", req.UserName, err)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"Error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"User": map[string]any{"Name": req.UserName, "Id": "fnos-user", "ServerId": h.ServerID,
			// Configuration 必须存在：官方 web 客户端登录后读 User.Configuration.ProfilePin
			// （undefined 直接 TypeError 中断登录流程）
			"Configuration": userConfiguration(),
			"Policy": map[string]any{
				"IsAdministrator":                true,
				"EnableAllFolders":               true,
				"EnableContentDeletion":          false,
				"EnableRemoteAccess":             true,
				"EnableLiveTvAccess":             false,
				"EnableMediaPlayback":            true,
				"EnableAudioPlaybackTranscoding": false,
				"EnableVideoPlaybackTranscoding": false,
				"EnablePlaybackRemuxing":         false,
				"RemoteClientBitrateLimit":       0,
			}},
		"SessionInfo": map[string]any{
			"UserId":   "fnos-user",
			"UserName": req.UserName,
			"Client":   "Emby",
			"Id":       "fnos-session",
			"ServerId": h.ServerID,
		},
		"ServerId":    h.ServerID,
		"AccessToken": embyToken,
		"Token":       embyToken,
		"UserSecret":  embyToken,
	})
}

func (h *Handler) handleUsersMe(w http.ResponseWriter, r *http.Request) {
	if !h.validClient(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{})
		return
	}
	// 官方 web 客户端读 user.Configuration.LatestItemsExcludes.includes(...)，
	// 这些字段必须是数组（undefined 直接 TypeError 打断首页 sections）
	writeJSON(w, http.StatusOK, map[string]any{
		"Name":          "fnos",
		"Id":            "fnos-user",
		"Configuration": userConfiguration(),
		"Policy": map[string]any{
			"MaxSimultaneousStreamingConnections": 10,
			"EnableMediaPlayback":                 true,
			"IsAdministrator":                     true,
		},
	})
}

// handleUsersByID 实现 GET /Users/{uid}：web 客户端 getUser(userId) 调用，
// 响应同 /Users/Me（Configuration 结构必须完整）。
func (h *Handler) handleUsersByID(w http.ResponseWriter, r *http.Request) {
	h.handleUsersMe(w, r)
}

// handleUsersPublic 实现 GET /Users/Public：官方 UserDto 纯数组（注意不是 QueryResult）。
func (h *Handler) handleUsersPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]any{
		{
			"Name":                  "admin",
			"Id":                    "fnos-user",
			"ServerId":              h.ServerID,
			"HasPassword":           true,
			"HasConfiguredPassword": true,
		},
	})
}

// handleItems 实现 Emby 的"列媒体库"。
// 0.9.8 真机契约：GET mediadb/list 直接列库（无需 SEED_GUIDS）；
// 库级浏览 POST item/list {ancestor_guid, tags.type, page/page_size}。
// 支持 Emby 标准分页参数 StartIndex/Limit（桥接层裁剪，TotalRecordCount 为全量数）。
func (h *Handler) handleItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parentID := queryGet(q, "ParentId")
	start := queryInt(q, "StartIndex", 0)
	limit := queryInt(q, "Limit", 0) // 0 = 不限

	writeItems := func(all []fn.MediaItem) {
		unfiltered := all
		// IncludeItemTypes 类型过滤（Emby 服务器 ASP.NET 大小写不敏感匹配；
		// 客户端按类型查询如 IncludeItemTypes=Episode/Movie/Series，不过滤
		// 会把混合列表原样返回=错误内容）。飞牛没有的实体类型 → 空列表。
		if types := strings.ToUpper(strings.TrimSpace(q.Get("IncludeItemTypes"))); types != "" {
			want := map[string]bool{}
			mapped := false
			for _, t := range strings.Split(types, ",") {
				switch strings.TrimSpace(t) {
				case "SERIES", "TVSHOW":
					want["TV"], mapped = true, true
				case "MOVIE":
					want["Movie"], mapped = true, true
				case "EPISODE":
					want["Episode"], mapped = true, true
				case "SEASON":
					want["Season"], mapped = true, true
				case "VIDEO":
					want["Video"], mapped = true, true
				case "FOLDER", "FOLDERITEM":
					want["MediaDB"], want["Directory"], want["TV"], want["Season"] = true, true, true, true
					mapped = true
				}
			}
			if !mapped {
				all = nil // 请求的类型飞牛全部不存在（MusicAlbum/PhotoAlbum 等）
			} else {
				var kept []fn.MediaItem
				for _, it := range all {
					if want[it.Type] {
						kept = append(kept, it)
					}
				}
				all = kept
			}
		}
		// fork 新增：库级浏览的空结果兜底。
		// 飞牛的库类别（TV/Movie/Other/Live）与 Emby 的类型体系不是一一对应，
		// 客户端按 CollectionType 猜出来的 IncludeItemTypes 可能整个对不上，
		// 表现为「点进媒体库一片空白，但首页能正常看到这些视频」。
		// 此时忽略类型过滤、返回该库的真实内容，比返回空列表更有用。
		// 仅在带 ParentId 的库级浏览生效，不影响全局筛选（如"所有电影"）。
		if len(all) == 0 && len(unfiltered) > 0 && parentID != "" {
			log.Printf("[items] IncludeItemTypes=%q 过滤后为空，库 %s 回退为不过滤（%d 条）",
				q.Get("IncludeItemTypes"), parentID, len(unfiltered))
			all = unfiltered
		}
		total := len(all)
		if start < 0 {
			start = 0
		}
		if start > total {
			start = total
		}
		page := all[start:]
		if limit > 0 && len(page) > limit {
			page = page[:limit]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"Items":            toEmbyItems(r, page, h),
			"TotalRecordCount": total,
			"StartIndex":       start,
		})
	}

	// 首页"播放列表/合集"等 section：客户端带 IncludeItemTypes=Playlist/BoxSet
	// 查询（无 ParentId）。飞牛无对应实体，之前直接把全库条目当合集返回（错误内容）。
	if types := strings.ToUpper(q.Get("IncludeItemTypes")); strings.Contains(types, "PLAYLIST") ||
		strings.Contains(types, "BOXSET") || strings.Contains(types, "COLLECTION") ||
		strings.Contains(types, "PERSON") || strings.Contains(types, "GENRE") {
		writeItems(nil)
		return
	}

	// 人物页"作品"网格：详情页点演员 → /Users/{uid}/Items?PersonIds={person_guid}
	// （飞牛端点 person/item/list，真机抓包）
	if pids := queryGet(q, "PersonIds"); pids != "" {
		guid := strings.TrimSpace(strings.Split(pids, ",")[0])
		list, _, err := h.fnOf(r.Context()).PersonItemList(r.Context(), guid, "", 1, 200)
		if err != nil {
			list = nil
		}
		writeItems(list)
		return
	}

	// 收藏列表：Filters=IsFavorite（详情页❤️标记后客户端收藏页拉取）
	if strings.Contains(strings.ToUpper(q.Get("Filters")), "ISFAVORITE") {
		list, _, err := h.fnOf(r.Context()).FavoriteList(r.Context(), 1, 200)
		if err != nil {
			list = nil
		}
		writeItems(list)
		return
	}

	// 已观看列表：Filters=IsPlayed（客户端"已观看"页/筛选）
	// 全库可播条目里筛 watched/is_watched，最近看的在前（watched_ts 降序）
	if strings.Contains(strings.ToUpper(q.Get("Filters")), "ISPLAYED") {
		var list []fn.MediaItem
		if dbs, err := h.fnOf(r.Context()).MediaDBList(r.Context()); err == nil {
			for _, db := range dbs {
				els, err := h.fnOf(r.Context()).ItemListTyped(r.Context(), db.Guid, []string{"Episode", "Movie", "Video"}, 0)
				if err != nil {
					continue
				}
				for _, m := range els {
					if m.Watched != 0 || m.IsWatched != 0 {
						list = append(list, m)
					}
				}
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].WatchedTs > list[j].WatchedTs })
		writeItems(list)
		return
	}

	if parentID != "" {
		recursive := strings.EqualFold(q.Get("Recursive"), "true")
		// 按父条目类型分派（真机确认 item/{guid} 对库/剧/季都有返回）：
		//   MediaDB(库) → 只列容器级（剧/电影）——"点进库看到整剧，点进剧才是季"
		//   TV(剧) → 季列表；Season(季) → 集列表；其他 → 自动双模式回退
		listMode := ""
		if det, err := h.fnOf(r.Context()).ItemDetail(r.Context(), parentID); err == nil && det != nil {
			listMode = det.Type
		}
		var (
			list []fn.MediaItem
			err  error
		)
		switch listMode {
		case "MediaDB":
			list, err = h.fnOf(r.Context()).ItemListContainers(r.Context(), parentID, 0)
		default: // TV/Season/未知（ItemDetail 失败时保持旧行为）
			list, err = h.fnOf(r.Context()).ItemList(r.Context(), parentID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"Error": err.Error(), "Items": []any{}})
			return
		}
		// Recursive=true：官方返回剧下全部集（Hills 剧详情"继续播放"区块、
		// "共 x 集"统计的数据源）——逐季平铺展开；非递归保持季/集列表
		if recursive && len(list) > 0 && list[0].Type == "Season" {
			list = h.seriesEpisodes(r.Context(), parentID)
		}
		// 官方默认排序：季按季号、集按集号升序（飞牛按添加时间 DESC，需重排）
		switch listMode {
		case "TV":
			sort.Slice(list, func(i, j int) bool { return list[i].SeasonNumber < list[j].SeasonNumber })
		case "Season":
			sort.Slice(list, func(i, j int) bool { return list[i].EpisodeNumber < list[j].EpisodeNumber })
		}
		writeItems(list)
		return
	}

	// 根级：mediadb/list 列出全部库，聚合各库条目
	dbs, err := h.fnOf(r.Context()).MediaDBList(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"Error": err.Error(), "Items": []any{}})
		return
	}
	var all []fn.MediaItem
	for _, db := range dbs {
		list, err := h.fnOf(r.Context()).ItemList(r.Context(), db.Guid)
		if err != nil {
			log.Printf("库 %q item/list: %v", db.Guid, err)
			continue
		}
		all = append(all, list...)
	}
	writeItems(all)
}

// parseInt 解析正整数，非法/负值返回 0。
func parseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// queryGet 大小写不敏感取 query 参数（Emby 服务器 ASP.NET 默认大小写不敏感，
// 客户端可能传 SeasonId/seasonId 等任意大小写组合）。
func queryGet(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	lk := strings.ToLower(key)
	for k, vs := range q {
		if strings.ToLower(k) == lk && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// queryInt 从 query 取整数，缺省/非法返回 def。
func queryInt(q map[string][]string, key string, def int) int {
	if v, ok := q[key]; ok && len(v) > 0 {
		n := 0
		if _, err := fmt.Sscanf(v[0], "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// handleViews 实现 GET /Users/{uid}/Views：Emby 客户端首页的媒体库视图列表。
// 直接映射飞牛 mediadb/list（0.9.8 原生支持），CollectionType 按 category 映射。
func (h *Handler) handleViews(w http.ResponseWriter, r *http.Request) {
	dbs, err := h.fnOf(r.Context()).MediaDBList(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"Error": err.Error(), "Items": []any{}})
		return
	}
	items := []map[string]any{}
	for _, db := range dbs {
		item := map[string]any{
			"Id":                      db.Guid,
			"Name":                    db.Title,
			"Type":                    "CollectionFolder",
			"IsFolder":                true,
			"CanDelete":               false,
			"ServerId":                h.ServerID,
			"ImageTags":               map[string]any{"Primary": "Primary"},
			"PrimaryImageAspectRatio": 0.6667,
		}
		if cat := collectionType(db.Category); cat != "" {
			item["CollectionType"] = cat
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Items":            items,
		"TotalRecordCount": len(items),
		"StartIndex":       0,
	})
}

// collectionType 把飞牛库类别映射成 Emby CollectionType。
//
// 飞牛库类别只有 TV / Movie / Other / Live 四种（见 fn.MediaDB.Category 注释）。
// 其中 Other（飞牛界面里叫「其他视频」）在 Emby 里没有对应类型，按 Emby 官方定义
// 归为**混合内容**：
//
//	"If CollectionType is null, it indicates a mixed movie/tv folder
//	 that should be displayed generically."   —— Emby REST API 文档
//
// 也就是返回空串、不声明 CollectionType。
//
// ⚠️ 不要写成 "mixed"：Emby 文档列出的可用值（movies/tvshows/music/games/books/
// musicvideos/homevideos/livetv/channels）里没有 mixed，Jellyfin 的 CollectionType
// 响应枚举同样没有（mixed 只存在于建库选项 CollectionTypeOptions）。上报未知值
// 会让部分客户端直接加载不了该媒体库。
//
// 混合库会让客户端自行猜测 IncludeItemTypes（多为 Movie,Series），而 Other 类库的
// 条目是 Type=Video —— 猜错就返回 0 条、点进库一片空白。这一层在 handleItems 的
// 空结果兜底里解决，不需要伪造 CollectionType。
func collectionType(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "movie", "movies", "电影":
		return "movies"
	case "tvshow", "tvshows", "tv", "剧集":
		return "tvshows"
	}
	return ""
}

func (h *Handler) handleItemByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 详情用 item/{guid}：对 TV/Season 等容器类型也能正确返回（play/info 只适用于可播条目）
	item, err := h.fnOf(r.Context()).ItemDetail(r.Context(), id)
	if err != nil {
		// 人物 guid（详情页 People 注入的 Id 非条目）：条目 404 → 演员详情
		// （Yamby/Hills 详情页点演员进人物页走 /Users/{uid}/Items/{personId}）
		if pd, perr := h.fnOf(r.Context()).PersonDetail(r.Context(), id); perr == nil && pd.Guid != "" {
			writeJSON(w, http.StatusOK, personToEmby(pd))
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"Error": err.Error()})
		return
	}
	// 集详情：沿 parent 链解析剧 guid（集.parent=季，季.parent=剧）。
	// 之前详情场景 SeriesId 缺失（列表场景由下钻参数传入）——Flutter 客户端
	// 集详情页 seriesId! 强解包直接 "Null check operator used on a null value"
	seriesID := ""
	if item.Type == "Episode" && item.ParentGuid != "" {
		if season, err := h.fnOf(r.Context()).ItemDetail(r.Context(), item.ParentGuid); err == nil {
			seriesID = season.ParentGuid
		}
	}
	out := toEmbyItemWithSeries(r, *item, seriesID, h)
	// 可播条目：拉真实媒体数据（文件大小/码率/流声明）填 MediaSourceInfo，
	// Yamby 详情页媒体信息区块直接读这些字段（缺了显示 0B/0bps/0分0秒）
	switch item.Type {
	case "Episode", "Movie", "Video":
		if item.CanPlay != 0 {
			h.enrichMediaSources(r, out, id)
		}
	case "TV", "Season":
		// 官方剧/季详情带累计总时长（全部集之和）+ 递归集数。
		// 飞牛剧/季条目无时长字段（碧蓝之海 TV runtime=None）——拉集列表累加补齐，
		// 否则客户端剧详情页不显示"总时长 xx 小时"
		if total, count := h.seriesTotals(r.Context(), item.Guid); total > 0 {
			out["RunTimeTicks"] = total * 10_000_000
			if count > 0 {
				out["RecursiveItemCount"] = count
			}
		}
	}
	// 演员表（详情页"演员"区）：飞牛 item/{guid} 详情不带演职员，
	// 拆在独立端点 person/list/{guid}（真机抓包）。真机验证（上野/碧蓝）：
	// 演员表挂"季"级——剧 guid 与集 guid 查询均 0 人，季 guid 返回全部演员。
	switch item.Type {
	case "TV", "Movie", "Video":
		h.injectPeople(r, out, item.Guid, item.Type == "TV")
	case "Season":
		h.injectPeople(r, out, item.Guid, false) // 季 guid 直接命中
	case "Episode":
		// 集 guid 0 人：用所属季 guid，空再回退剧 guid（内部逐季兜底）
		if item.ParentGuid != "" {
			h.injectPeople(r, out, item.ParentGuid, false)
		} else if seriesID != "" {
			h.injectPeople(r, out, seriesID, true)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// peopleEntry / peopleCache 演员表 60s 缓存（剧详情页 Items/NextUp/总时长 +
// People 多路触发，避免重复打 person/list）。
type peopleEntry struct {
	list []fn.Person
	at   time.Time
}

var peopleCache sync.Map // guid -> peopleEntry

// peopleFor 取 guid 演员表（带缓存）。
func (h *Handler) peopleFor(ctx context.Context, guid string) []fn.Person {
	if v, ok := peopleCache.Load(guid); ok {
		if e, ok := v.(peopleEntry); ok && time.Since(e.at) < 60*time.Second {
			return e.list
		}
	}
	list, err := h.fnOf(ctx).PersonList(ctx, guid)
	if err != nil {
		log.Printf("[people] person/list %q: %v", guid, err)
		return nil
	}
	peopleCache.Store(guid, peopleEntry{list: list, at: time.Now()})
	return list
}

// injectPeople 把飞牛 演员表 映射成 Emby People 注入详情 item。
// isSeries=true（剧）：飞牛演员表挂"季"级，剧 guid 查询为空时逐季找第一个非空季。
func (h *Handler) injectPeople(r *http.Request, out map[string]any, guid string, isSeries bool) {
	list := h.peopleFor(r.Context(), guid)
	if len(list) == 0 && isSeries {
		if seasons, err := h.fnOf(r.Context()).SeasonList(r.Context(), guid); err == nil {
			for _, s := range seasons {
				if l := h.peopleFor(r.Context(), s.Guid); len(l) > 0 {
					list = l
					break
				}
			}
		}
	}
	if len(list) == 0 {
		return
	}
	out["People"] = embyPeople(list)
}

// embyPeople 飞牛 Person → Emby BaseItemDto.People 元素。
// 头像走无状态方案：PrimaryImageTag = base64url(profile_path)，
// handleItemImage 解码 tag 直接代理 sys/img（人物 guid 非条目，查 ItemDetail 必 404）。
func embyPeople(list []fn.Person) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		m := map[string]any{
			"Id":           p.PersonGuid,
			"Name":         p.Name,
			"OriginalName": p.OriginalName,
			"Role":         p.Role,
			"Type":         personType(p.Job),
			// Emby 官方竖版头像宽高比（2:3），客户端按此排布局
			"PrimaryImageAspectRatio": 0.6666666666666666,
		}
		if p.ProfilePath != "" {
			m["PrimaryImageTag"] = base64.URLEncoding.EncodeToString([]byte(p.ProfilePath))
		}
		out = append(out, m)
	}
	return out
}

// personType 飞牛 job → Emby People.Type。
func personType(job string) string {
	switch strings.ToLower(job) {
	case "actor", "":
		return "Actor"
	case "director":
		return "Director"
	case "writer", "screenplay", "story":
		return "Writer"
	case "composer":
		return "Composer"
	case "producer":
		return "Producer"
	default:
		return job
	}
}

// cachedEpisodes 剧/季下全部集（展开+排序），60s TTL 缓存——
// 剧详情页一次会触发 NextUp/递归 Items/总时长 三路拉取，不缓存会重复翻季。
type episodesEntry struct {
	eps []fn.MediaItem
	at  time.Time
}

var episodesCache sync.Map // guid -> episodesEntry

// seriesEpisodes 拉剧/季下全部集（剧 guid 自动逐季展开），按季号/集号升序。
func (h *Handler) seriesEpisodes(ctx context.Context, guid string) []fn.MediaItem {
	if v, ok := episodesCache.Load(guid); ok {
		if e, ok := v.(episodesEntry); ok && time.Since(e.at) < 60*time.Second {
			return e.eps
		}
	}
	// 优先 episode/list（真机抓包：剧 guid 一次返回全剧所有集，跨季平铺自带
	// ts/duration/watched）——比"剧→逐季 item/list"少 N 次请求；
	// 季 guid 或端点异常时回退旧逻辑（season 展开）
	if eps, err := h.fnOf(ctx).EpisodeList(ctx, guid); err == nil && len(eps) > 0 {
		sort.Slice(eps, func(i, j int) bool {
			if eps[i].SeasonNumber != eps[j].SeasonNumber {
				return eps[i].SeasonNumber < eps[j].SeasonNumber
			}
			return eps[i].EpisodeNumber < eps[j].EpisodeNumber
		})
		episodesCache.Store(guid, episodesEntry{eps: eps, at: time.Now()})
		return eps
	}
	eps, err := h.fnOf(ctx).ItemList(ctx, guid)
	if err != nil {
		return nil
	}
	// 剧 guid：item/list 返回季列表 → 逐季展开
	if len(eps) > 0 && eps[0].Type == "Season" {
		var all []fn.MediaItem
		for _, s := range eps {
			el, err := h.fnOf(ctx).ItemList(ctx, s.Guid)
			if err != nil {
				continue
			}
			all = append(all, el...)
		}
		eps = all
	}
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].SeasonNumber != eps[j].SeasonNumber {
			return eps[i].SeasonNumber < eps[j].SeasonNumber
		}
		return eps[i].EpisodeNumber < eps[j].EpisodeNumber
	})
	episodesCache.Store(guid, episodesEntry{eps: eps, at: time.Now()})
	return eps
}

// seriesTotals 汇总剧/季全部集时长（秒）与集数（复用 seriesEpisodes 缓存）。
func (h *Handler) seriesTotals(ctx context.Context, guid string) (int64, int) {
	eps := h.seriesEpisodes(ctx, guid)
	var total int64
	for _, ep := range eps {
		switch {
		case ep.Duration > 0:
			total += int64(ep.Duration)
		case ep.Runtime > 0:
			total += int64(ep.Runtime) * 60
		}
	}
	return total, len(eps)
}

func mediaVersionLabel(file fn.StreamFile, vs *fn.StreamListStream) string {
	if vs != nil {
		resolution := strings.ToUpper(vs.ResolutionType)
		if vs.Height >= 2160 {
			resolution = "4K"
		} else if vs.Height > 0 {
			resolution = fmt.Sprintf("%dp", vs.Height)
		}
		codec := strings.ToUpper(vs.CodecName)
		switch {
		case resolution != "" && codec != "":
			return resolution + " " + codec
		case resolution != "":
			return resolution
		case codec != "":
			return codec
		}
	}
	if file.FileName != "" {
		if file.Size > 0 {
			label := strings.TrimSuffix(file.FileName, filepath.Ext(file.FileName))
			return label + " · " + humanSize(file.Size)
		}
		return file.FileName
	}
	return file.Guid
}

func humanSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%dB", size)
	}
	value := float64(size)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, suffix := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f%s", value, suffix)
		}
	}
	return fmt.Sprintf("%.2fPB", value/unit)
}

func displayLanguage(code string) string {
	switch strings.ToLower(code) {
	case "chi", "zho":
		return "中文"
	case "eng":
		return "英语"
	case "jpn":
		return "日语"
	case "kor":
		return "韩语"
	case "ind":
		return "印尼语"
	case "may", "msa":
		return "马来语"
	case "tha":
		return "泰语"
	case "vie":
		return "越南语"
	case "":
		return "未知"
	default:
		return strings.ToUpper(code)
	}
}

// enrichMediaSources 用 stream/list 的真实版本列表填充 MediaSourceInfo。
// 失败时回退 play/info + stream 的单版本路径（详情页不至于崩）。
func (h *Handler) enrichMediaSources(r *http.Request, item map[string]any, guid string) {
	ctx := r.Context()
	ms, ok := item["MediaSources"].([]map[string]any)
	if !ok || len(ms) == 0 {
		return
	}
	src := ms[0]
	playURL := h.streamURL(r, guid)
	src["Path"] = playURL
	src["DirectStreamUrl"] = playURL
	info, err := h.fnOf(ctx).PlayInfoByGuid(ctx, guid)
	if err != nil {
		log.Printf("[media-info] play/info %s: %v", guid, err)
		return
	}
	// 飞牛 item/{guid} 单条详情接口不带 ts（播放位置）字段——只有 item/list 列表接口带。
	// 客户端集详情页的"继续播放"读这里的 UserData：不补就永远显示从头播
	// （"剧集详情页能继续观看、点进单集反而不能"的根因）。
	if ud, ok := item["UserData"].(map[string]any); ok {
		if info.Ts > 0 {
			ud["PlaybackPositionTicks"] = info.Ts * 10_000_000
		}
		if wt := info.Item.WatchedTs; wt > 1_000_000_000 {
			ud["LastPlayedDate"] = time.Unix(wt, 0).UTC().Format("2006-01-02T15:04:05.0000000Z")
		}
		if played := info.Item.IsWatched != 0; played {
			ud["Played"] = true
			ud["PlayCount"] = 1
		}
		// 进度百分比：item/{guid} 单详情无 duration（toEmbyItemWithSeries 里
		// total=0 算不出比例）→ 用 play/info 时长补条目总时长 + 百分比，
		// 集详情页进度条才能显示"已看 x%"
		if total := playInfoSeconds(info); total > 0 {
			item["RunTimeTicks"] = total * 10_000_000
			src["RunTimeTicks"] = total * 10_000_000
			if info.Ts > 0 && info.Item.IsWatched == 0 {
				ud["PlayedPercentage"] = float64(info.Ts) / float64(total) * 100
			}
		}
	}
	list, listErr := h.fnOf(ctx).StreamListByGuid(ctx, guid)
	if listErr != nil || len(list.Files) == 0 {
		log.Printf("[media-info] stream/list %s: %v", guid, listErr)
		h.enrichSingleMediaSource(r, item, src, info)
		return
	}

	byMedia := make(map[string][]fn.StreamListStream, len(list.VideoStreams))
	for _, vs := range list.VideoStreams {
		byMedia[vs.MediaGuid] = append(byMedia[vs.MediaGuid], vs)
	}
	audiosByMedia := make(map[string][]fn.StreamListStream, len(list.AudioStreams))
	for _, as := range list.AudioStreams {
		audiosByMedia[as.MediaGuid] = append(audiosByMedia[as.MediaGuid], as)
	}
	subtitlesByMedia := make(map[string][]fn.SubtitleStream, len(list.SubtitleStreams))
	for _, ss := range list.SubtitleStreams {
		subtitlesByMedia[ss.MediaGuid] = append(subtitlesByMedia[ss.MediaGuid], ss)
	}
	videoIndex := 0
	for _, file := range list.Files {
		if file.CanPlay == 0 || file.Guid == "" {
			continue
		}
		videos := byMedia[file.Guid]
		var vs *fn.StreamListStream
		if len(videos) > 0 {
			vs = &videos[0]
		}
		fileURL := h.streamURL(r, guid) + "&Version=" + file.Guid
		msrc := map[string]any{
			"Id":                   file.Guid,
			"MediaSourceId":        file.Guid,
			"Name":                 mediaVersionLabel(file, vs),
			"Path":                 fileURL,
			"DirectStreamUrl":      fileURL,
			"Protocol":             "Http",
			"Type":                 "Virtual",
			"IsRemote":             true,
			"SupportsDirectPlay":   true,
			"SupportsDirectStream": true,
			"SupportsTranscoding":  false,
			"SupportsProbing":      false,
			"RequiresOpening":      false,
			"Container":            strings.TrimPrefix(strings.ToLower(filepath.Ext(file.FileName)), "."),
			"Size":                 file.Size,
			"MediaStreams":         []map[string]any{},
		}
		if file.CreateTime > 0 {
			msrc["DateCreated"] = time.UnixMilli(file.CreateTime).UTC().Format("2006-01-02T15:04:05.0000000Z")
		}
		if vs != nil {
			if vs.Duration > 0 {
				msrc["RunTimeTicks"] = int64(vs.Duration) * 10_000_000
			}
			if vs.Bps > 0 {
				msrc["Bitrate"] = vs.Bps
			}
			s := map[string]any{"Type": "Video", "Index": 0, "IsDefault": true, "IsExternal": false}
			if vs.CodecName != "" {
				s["Codec"] = vs.CodecName
			}
			if vs.Width > 0 {
				s["Width"] = vs.Width
			}
			if vs.Height > 0 {
				s["Height"] = vs.Height
			}
			if vs.Bps > 0 {
				s["BitRate"] = vs.Bps
			}
			if vs.ColorRangeType != "" {
				s["VideoRange"] = vs.ColorRangeType
			}
			if vs.BitDepth > 0 {
				s["BitDepth"] = vs.BitDepth
			}
			if vs.PixelFmt != "" {
				s["PixelFormat"] = vs.PixelFmt
			}
			if vs.Aspect != "" {
				s["AspectRatio"] = vs.Aspect
			}
			if fps := parseFPS(vs.FrameRate); fps > 0 {
				s["RealFrameRate"] = fps
				s["AverageFrameRate"] = fps
			}
			if vs.Profile != "" {
				s["Profile"] = vs.Profile
			}
			if label := mediaVersionLabel(file, vs); label != "" {
				s["DisplayTitle"] = label
				s["Title"] = label
			}
			fillMediaStream(s)
			streams := []map[string]any{s}
			for ai, as := range audiosByMedia[file.Guid] {
				a := map[string]any{
					"Type":       "Audio",
					"Index":      as.Index,
					"IsExternal": false,
				}
				if as.Index == 0 {
					a["Index"] = ai + 1
				}
				if as.CodecName != "" {
					a["Codec"] = as.CodecName
				}
				if as.Language != "" {
					a["Language"] = as.Language
				}
				if as.Profile != "" {
					a["Profile"] = as.Profile
				}
				if as.Bps > 0 {
					a["BitRate"] = as.Bps
				}
				if as.Channels > 0 {
					a["Channels"] = as.Channels
				}
				if as.SampleRate != "" {
					if n, err := strconv.Atoi(as.SampleRate); err == nil {
						a["SampleRate"] = n
					}
				}
				if as.ChannelLayout != "" {
					a["ChannelLayout"] = as.ChannelLayout
				}
				if as.Duration > 0 {
					a["Duration"] = fmt.Sprintf("%.6f", float64(as.Duration))
				}
				a["IsDefault"] = as.IsDefault != 0
				a["IsForced"] = false
				lang := displayLanguage(as.Language)
				title := as.Title
				if title == "" {
					title = lang + " " + strings.ToUpper(as.CodecName)
				}
				a["DisplayLanguage"] = lang
				// fork: 对手机普遍无法直解的编码（DTS/TrueHD/FLAC…）在标题里加提示，
				// 让用户能手动挑一条兼容音轨；不影响任何播放决策。
				a["DisplayTitle"] = audioDisplayTitle(title, as.CodecName)
				a["Title"] = title
				fillMediaStream(a)
				streams = append(streams, a)
			}
			for _, ss := range subtitlesByMedia[file.Guid] {
				sub := map[string]any{
					"Type":       "Subtitle",
					"Index":      ss.Index,
					"IsExternal": ss.IsExternal != 0,
					"Codec":      ss.CodecName,
					"Language":   ss.Language,
					"Title":      ss.Title,
					"IsDefault":  ss.IsDefault != 0,
					"IsForced":   false,
					"IsBitmap":   ss.IsBitmap != 0,
				}
				if sub["Codec"] == "subrip" {
					sub["Codec"] = "srt"
				}
				lang := displayLanguage(ss.Language)
				if sub["Title"] == "" || sub["Title"] == ss.Language {
					sub["Title"] = lang + "字幕"
				}
				sub["DisplayLanguage"] = lang
				sub["DisplayTitle"] = sub["Title"]
				sub["IsTextSubtitleStream"] = ss.IsBitmap == 0
				sub["SupportsExternalStream"] = ss.IsExternal != 0
				fillMediaStream(sub)
				streams = append(streams, sub)
			}
			msrc["MediaStreams"] = streams
			if len(audiosByMedia[file.Guid]) > 0 {
				msrc["DefaultAudioStreamIndex"] = streams[1]["Index"]
			}
			for i, ss := range subtitlesByMedia[file.Guid] {
				if ss.IsDefault != 0 {
					msrc["DefaultSubtitleStreamIndex"] = streams[1+len(audiosByMedia[file.Guid])+i]["Index"]
					break
				}
			}
		}
		fillMediaSource(msrc)
		if videoIndex < len(ms) {
			ms[videoIndex] = msrc
		} else {
			ms = append(ms, msrc)
		}
		videoIndex++
	}
	if videoIndex == 0 {
		h.enrichSingleMediaSource(r, item, src, info)
		return
	}
	item["MediaSources"] = ms[:videoIndex]
	if m, ok := ms[0]["Id"].(string); ok {
		item["MediaSourceId"] = m
	}
}

// enrichSingleMediaSource 保留旧的单版本增强逻辑：stream/list 不可用或无文件时兜底。
func (h *Handler) enrichSingleMediaSource(r *http.Request, item map[string]any, src map[string]any, info *fn.PlayInfo) {
	ctx := r.Context()
	guid := info.Guid
	playURL := h.streamURL(r, guid)
	src["Path"] = playURL
	src["DirectStreamUrl"] = playURL
	if ud, ok := item["UserData"].(map[string]any); ok {
		if info.Ts > 0 {
			ud["PlaybackPositionTicks"] = info.Ts * 10_000_000
		}
		if wt := info.Item.WatchedTs; wt > 1_000_000_000 {
			ud["LastPlayedDate"] = time.Unix(wt, 0).UTC().Format("2006-01-02T15:04:05.0000000Z")
		}
		if played := info.Item.IsWatched != 0; played {
			ud["Played"] = true
			ud["PlayCount"] = 1
		}
		if total := playInfoSeconds(info); total > 0 {
			item["RunTimeTicks"] = total * 10_000_000
			src["RunTimeTicks"] = total * 10_000_000
			if info.Ts > 0 && info.Item.IsWatched == 0 {
				ud["PlayedPercentage"] = float64(info.Ts) / float64(total) * 100
			}
		}
	}
	resp, err := h.fnOf(ctx).Stream(ctx, info.MediaGuid, "")
	if err != nil {
		log.Printf("[media-info] stream %s: %v", info.MediaGuid, err)
		return
	}
	// 入库时间（覆盖骨架的首播日兜底，官方媒体信息显示的是入库时间）
	// → 官方 ISO round-trip 格式（Yamby 强类型日期解析要求）
	if ts := resp.FileStream.CreateTime; ts > 0 {
		item["DateCreated"] = time.UnixMilli(ts).UTC().Format("2006-01-02T15:04:05.0000000Z")
	}
	if sz := resp.FileStream.Size; sz > 0 {
		src["Size"] = sz
		item["Size"] = sz
	}
	if n := resp.FileStream.FileName; n != "" {
		src["Name"] = n
		// 真实容器：取文件扩展名（客户端媒体信息显示容器名）
		if i := strings.LastIndex(n, "."); i >= 0 && i < len(n)-1 {
			ext := strings.ToLower(n[i+1:])
			src["Container"] = ext
			item["Container"] = ext
		}
	}
	duration := 0
	if resp.VideoStream != nil {
		duration = resp.VideoStream.Duration
	}
	if duration > 0 {
		ticks := int64(duration) * 10_000_000
		src["RunTimeTicks"] = ticks
		item["RunTimeTicks"] = ticks
	}
	// 码率：直链质量位优先，兜底按 大小×8/时长 估算
	bitrate := 0
	if len(resp.DirectLinkQualities) > 0 {
		bitrate = resp.DirectLinkQualities[0].Bitrate
	}
	if bitrate == 0 {
		if sz, _ := src["Size"].(int64); sz > 0 && duration > 0 {
			bitrate = int(sz*8) / duration
		}
	}
	if bitrate > 0 {
		src["Bitrate"] = bitrate
		item["Bitrate"] = bitrate
	}
	// MediaStreams：对齐官方 Emby 媒体信息面板字段（Yamby 逐流渲染：
	// 码率/动态范围/位深/像素格式/长宽比/帧率/显示语言 全部来自这里）
	streams := []map[string]any{}
	if vs := resp.VideoStream; vs != nil {
		s := map[string]any{"Type": "Video", "Index": 0, "IsDefault": true, "IsExternal": false}
		if vs.CodecName != "" {
			s["Codec"] = vs.CodecName
		}
		if vs.Width > 0 {
			s["Width"] = vs.Width
		}
		if vs.Height > 0 {
			s["Height"] = vs.Height
		}
		if vs.Bps > 0 {
			s["BitRate"] = vs.Bps
		}
		if vs.ColorRangeType != "" {
			s["VideoRange"] = vs.ColorRangeType // SDR / HDR（官方字段名）
		}
		if vs.BitDepth > 0 {
			s["BitDepth"] = vs.BitDepth
		}
		if vs.PixelFmt != "" {
			s["PixelFormat"] = vs.PixelFmt
		}
		if vs.Aspect != "" {
			s["AspectRatio"] = vs.Aspect
		}
		if fps := parseFPS(vs.FrameRate); fps > 0 {
			s["RealFrameRate"] = fps
			s["AverageFrameRate"] = fps
		}
		if vs.Profile != "" {
			s["Profile"] = vs.Profile
		}
		// DisplayTitle 官方形态 "4K HEVC"（分辨率档 + 编码大写）
		disp := ""
		if vs.Height > 0 {
			res := fmt.Sprintf("%dp", vs.Height)
			if vs.Height >= 2160 {
				res = "4K"
			}
			disp = res
			if vs.CodecName != "" {
				disp += " " + strings.ToUpper(vs.CodecName)
			}
		}
		if disp != "" {
			s["DisplayTitle"] = disp
			s["Title"] = disp
		}
		fillMediaStream(s)
		streams = append(streams, s)
	}
	for i, raw := range resp.AudioStreams {
		var a struct {
			CodecName      string `json:"codec_name"`
			Title          string `json:"title"`
			Language       string `json:"language"`
			Channels       int    `json:"channels"`
			ChannelLayout  string `json:"channel_layout"` // stereo / 5.1
			SampleRate     string `json:"sample_rate"`    // "48000"
			BitsPerRawSnap string `json:"bits_per_raw_sample"`
			Bps            int64  `json:"bps"`
			IsDefault      int    `json:"is_default"`
		}
		_ = json.Unmarshal(raw, &a)
		s := map[string]any{"Type": "Audio", "Index": i + 1, "IsDefault": a.IsDefault != 0 || i == 0, "IsExternal": false}
		if a.CodecName != "" {
			s["Codec"] = a.CodecName
		}
		if a.Language != "" && a.Language != "zz-unknow" {
			s["Language"] = a.Language
		}
		// DisplayLanguage：官方存语言全名（Yamby "显示语言: Japanese" 行）
		if full := langName(a.Language); full != "" {
			s["DisplayLanguage"] = full
		}
		if a.Channels > 0 {
			s["Channels"] = a.Channels
		}
		if a.ChannelLayout != "" {
			s["ChannelLayout"] = a.ChannelLayout
		}
		if sr := parseInt(a.SampleRate); sr > 0 {
			s["SampleRate"] = sr
		}
		if bd := parseInt(a.BitsPerRawSnap); bd > 0 {
			s["BitDepth"] = bd
		}
		if a.Bps > 0 {
			s["BitRate"] = a.Bps
		}
		// DisplayTitle 官方形态 "Japanese FLAC (默认)"：语言全名 + 编码大写 + 默认轨标记
		disp := langName(a.Language)
		if a.CodecName != "" {
			if disp != "" {
				disp += " "
			}
			disp += strings.ToUpper(a.CodecName)
		}
		if disp != "" {
			if a.IsDefault != 0 || i == 0 {
				disp += " (默认)"
			}
			s["DisplayTitle"] = disp
			s["Title"] = disp
		}
		fillMediaStream(s)
		streams = append(streams, s)
	}
	// 字幕流：Yamby 字幕菜单与详情页字幕行都读这里；
	// mkv 内嵌轨（is_external=0）在 mpv 直连播放时由内核直接读取，
	// 声明是必须的（缺了客户端认为无字幕 → "字幕没有"）
	for _, ss := range resp.SubtitleStreams {
		s := map[string]any{
			"Type":                   "Subtitle",
			"Index":                  ss.Index,
			"IsDefault":              ss.IsDefault != 0,
			"IsExternal":             ss.IsExternal != 0,
			"IsTextSubtitleStream":   ss.IsBitmap == 0,
			"SupportsExternalStream": ss.IsExternal != 0 && ss.IsBitmap == 0,
		}
		if ss.CodecName != "" {
			s["Codec"] = ss.CodecName
		}
		if ss.Format != "" {
			s["SubtitleLocationType"] = ss.Format
		}
		if ss.Language != "" && ss.Language != "zz-unknow" {
			s["Language"] = ss.Language
		}
		if full := langName(ss.Language); full != "" {
			s["DisplayLanguage"] = full
		}
		// DisplayTitle 官方形态 "English - ASS"
		disp := langName(ss.Language)
		if disp == "" && ss.Title != "" {
			disp = ss.Title
		}
		if ss.CodecName != "" {
			if disp != "" {
				disp += " - "
			}
			disp += strings.ToUpper(ss.CodecName)
		}
		if disp != "" {
			s["DisplayTitle"] = disp
			s["Title"] = disp
		}
		fillMediaStream(s)
		streams = append(streams, s)
	}
	if len(streams) > 0 {
		src["MediaStreams"] = streams
	}
	fillMediaSource(src)
}

// nullableDate 日期字段无值时输出 null（官方语义），空串会让
// Dart/Flutter 客户端 DateTime.parse("") 抛 "FormatException: Invalid date format"
// （Hills 真机实证），null 才是官方对无值日期的形态。
func nullableDate(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fillMediaStream 把单条 MediaStream 补齐到官方形态。Yamby 用
// kotlinx.serialization 强类型解析，MediaStream.IsForced 等布尔字段为
// 非空无默认值属性——缺失即 SerializationException（真机崩溃截图实证：
// "Field 'IsForced' is required"），页面直接 Oops。字段清单对照真机
// Emby 4.9 抓包（HAR）。未知字段 Yamby 会忽略（主页 Resume 带
// MediaSources 仍正常渲染），因此只补不删、全给安全值。
func fillMediaStream(s map[string]any) {
	bools := []string{"IsAnamorphic", "IsDefault", "IsExternal", "IsForced",
		"IsHearingImpaired", "IsInterlaced", "IsTextSubtitleStream", "SupportsExternalStream"}
	for _, k := range bools {
		if _, ok := s[k]; !ok {
			s[k] = false
		}
	}
	nums := []string{"AttachmentSize", "BitDepth", "BitRate", "Channels", "Height",
		"Index", "Level", "RefFrames", "SampleRate", "Width"}
	for _, k := range nums {
		if _, ok := s[k]; !ok {
			s[k] = 0
		}
	}
	strs := []string{"AspectRatio", "Codec", "DisplayTitle", "DisplayLanguage",
		"ExtendedVideoSubType", "ExtendedVideoSubTypeDescription", "ExtendedVideoType",
		"Language", "PixelFormat", "Profile", "Protocol", "TimeBase", "Title", "VideoRange"}
	for _, k := range strs {
		if _, ok := s[k]; !ok {
			s[k] = ""
		}
	}
	if v, ok := s["RealFrameRate"]; !ok || v == 0 {
		if _, ok := s["RealFrameRate"]; !ok {
			s["RealFrameRate"] = 0.0
		}
	}
	if _, ok := s["AverageFrameRate"]; !ok {
		s["AverageFrameRate"] = 0.0
	}
}

// fillMediaSource 把 MediaSourceInfo 补齐到官方形态（字段对照真机 Emby 抓包）。
func fillMediaSource(src map[string]any) {
	bools := []string{"AddApiKeyToDirectStreamUrl", "HasMixedProtocols",
		"IsInfiniteStream", "ReadAtNativeFramerate", "RequiresClosing",
		"RequiresLooping", "RequiresOpening", "SupportsDirectPlay",
		"SupportsDirectStream", "SupportsProbing", "SupportsTranscoding"}
	for _, k := range bools {
		if _, ok := src[k]; !ok {
			src[k] = false
		}
	}
	if _, ok := src["DefaultAudioStreamIndex"]; !ok {
		src["DefaultAudioStreamIndex"] = 0
	}
	if _, ok := src["DefaultSubtitleStreamIndex"]; !ok {
		src["DefaultSubtitleStreamIndex"] = 0
	}
	if _, ok := src["Formats"]; !ok {
		src["Formats"] = []any{}
	}
	if _, ok := src["Chapters"]; !ok {
		src["Chapters"] = []any{}
	}
	if _, ok := src["RequiredHttpHeaders"]; !ok {
		src["RequiredHttpHeaders"] = map[string]any{}
	}
	if _, ok := src["MediaStreams"]; !ok {
		src["MediaStreams"] = []any{}
	}
	strs := []string{"Container", "Id", "MimeType", "Name", "Path", "Protocol", "Type"}
	for _, k := range strs {
		if _, ok := src[k]; !ok {
			src[k] = ""
		}
	}
	if _, ok := src["ItemId"]; !ok {
		if id, ok := src["Id"]; ok {
			src["ItemId"] = id
		} else {
			src["ItemId"] = ""
		}
	}
	if _, ok := src["IsRemote"]; !ok {
		src["IsRemote"] = true
	}
}

// fillItemCommon 把 BaseItemDto 条目补齐到官方形态（字段对照真机抓包的
// 剧/集详情响应），防 kotlinx 必需字段缺失崩（同 fillMediaStream 说明）。
func fillItemCommon(item map[string]any) {
	if _, ok := item["SupportsSync"]; !ok {
		item["SupportsSync"] = false
	}
	if _, ok := item["LockData"]; !ok {
		item["LockData"] = false
	}
	if _, ok := item["LockedFields"]; !ok {
		item["LockedFields"] = []any{}
	}
	if _, ok := item["DisplayPreferencesId"]; !ok {
		item["DisplayPreferencesId"] = ""
	}
	if _, ok := item["DateModified"]; !ok {
		if dc, ok := item["DateCreated"]; ok {
			item["DateModified"] = dc
		} else {
			item["DateModified"] = ""
		}
	}
	if _, ok := item["PresentationUniqueKey"]; !ok {
		item["PresentationUniqueKey"] = ""
	}
	if _, ok := item["ForcedSortName"]; !ok {
		item["ForcedSortName"] = ""
	}
	if _, ok := item["PartCount"]; !ok {
		item["PartCount"] = 0
	}
	if _, ok := item["LocalTrailerCount"]; !ok {
		item["LocalTrailerCount"] = 0
	}
	if _, ok := item["People"]; !ok {
		item["People"] = []any{}
	}
	if _, ok := item["RemoteTrailers"]; !ok {
		item["RemoteTrailers"] = []any{}
	}
	if _, ok := item["TagItems"]; !ok {
		item["TagItems"] = []any{}
	}
	if _, ok := item["ParentBackdropImageTags"]; !ok {
		item["ParentBackdropImageTags"] = []any{}
	}
	if _, ok := item["ParentBackdropItemId"]; !ok {
		item["ParentBackdropItemId"] = ""
	}
	if _, ok := item["FileName"]; !ok {
		if n, ok := item["Name"]; ok {
			item["FileName"] = n
		} else {
			item["FileName"] = ""
		}
	}
}

// streamURL 拼客户端可直接交给播放内核的绝对播放 URL。
// 关键：Yamby 等客户端会把 MediaSource.Path 原样传给播放器（此前 fn:// 伪协议
// 导致播放器本地立即失败、零网络请求——"点了播放没反应/播不了"的真凶），
// 同时补官方字段 DirectStreamUrl（官方服务器直连场景必有）。
func (h *Handler) streamURL(r *http.Request, id string) string {
	token := r.Header.Get("X-Emby-Token")
	if token == "" {
		token = r.URL.Query().Get("api_key")
	}
	q := "MediaSourceId=" + id + "&Static=true"
	if token != "" {
		q += "&api_key=" + token
	}
	return h.baseURL(r) + "/Videos/" + id + "/stream?" + q
}

// baseURL 返回客户端可达的本桥接基础 URL：优先 X-Forwarded-Proto/Host（反代场景），
// 其次请求 Host，最后退回配置的 h.Host。
func (h *Handler) baseURL(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	if r.Host != "" {
		return scheme + "://" + r.Host
	}
	return "http://" + h.Host
}

// playInfoSeconds 从 PlayInfo 估算时长（秒）：item.duration 优先，
// 回退 runtime（分钟）×60。
func playInfoSeconds(info *fn.PlayInfo) int64 {
	if info.Item.Duration > 0 {
		return int64(info.Item.Duration)
	}
	return int64(info.Item.Runtime) * 60
}

// handlePlayback 实现 POST /Videos/{id}/Playback（老端点，新客户端走 PlaybackInfo）。
func (h *Handler) handlePlayback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, err := h.fnOf(r.Context()).PlayInfoByGuid(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"Error": err.Error()})
		return
	}
	streamURL := fmt.Sprintf("%s/emby/Videos/%s/stream?Static=true&MediaSourceId=%s&api_key=%s",
		h.baseURL(r), id, id, h.apiKey(r))
	runTicks := playInfoSeconds(info) * 10_000_000
	writeJSON(w, http.StatusOK, map[string]any{
		// Path 必须真实 URL：客户端直接交给播放器内核（fn:// 伪协议零请求失败）
		"MediaSource": map[string]any{
			"Id":            id,
			"Path":          streamURL,
			"Type":          "Virtual",
			"IsSubtitle":    false,
			"RunTimeTicks":  runTicks,
			"MediaStreams":  []map[string]any{},
			"MediaSourceId": id,
		},
		"StreamUrl":       streamURL,
		"DirectPlayLinks": []map[string]any{},
		"TranscodingInfo": map[string]any{},
		"MediaSourceId":   id,
	})
}

// handlePlaybackInfo 实现 POST /Items/{id}/PlaybackInfo —— Emby 官方客户端的播放入口。
// MediaSource 必须带**真实**流声明：Hills 点集路径会在客户端用 DeviceProfile 对
// MediaStreams 逐流匹配（codec/width/profile），匹配失败且 SupportsTranscoding=false
// 时判定"无可用播放方案"直接静默放弃（连播放器 UI 都不起）——
// 此前这里塞假流（Codec="h264,hevc" 逗号串）导致"点每集没反应"，
// 而"剧集继续播放"路径跳过 profile 匹配所以能播。
// 现复用详情页 enrichMediaSources 的真实流构建（单流单 codec + 宽高/位深/profile）。
func (h *Handler) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, err := h.fnOf(r.Context()).PlayInfoByGuid(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"Error": err.Error()})
		return
	}
	src := map[string]any{
		"Id":                   id,
		"MediaSourceId":        id,
		"ServerId":             h.ServerID,
		"Protocol":             "Http",
		"Type":                 "Virtual",
		"IsRemote":             true,
		"SupportsDirectPlay":   true,
		"SupportsDirectStream": true,
		"SupportsTranscoding":  false,
		"SupportsProbing":      false,
		"RequiresOpening":      false,
		"Container":            "mp4,mkv,mov,webm,avi,ts",
		"RunTimeTicks":         playInfoSeconds(info) * 10_000_000,
		"MediaStreams":         []map[string]any{},
	}
	item := map[string]any{"Id": id, "MediaSources": []map[string]any{src}}
	// 拉真实流数据（codec/分辨率/码率/容器/大小）覆盖骨架；Path/DirectStreamUrl 也在其中
	h.enrichMediaSources(r, item, id)
	writeJSON(w, http.StatusOK, map[string]any{
		// PlaySessionId 必须每次随机（官方为 GUID）。固定值会让 Yamby 把第二次
		// 播放判定为"会话已存在"而忽略点击——"第一次能播、第二次按钮无反应"
		"PlaySessionId": fmt.Sprintf("fnos-%s-%08x", id[:12], randUint32()),
		"ErrorCode":     nil,
		"MediaSources":  item["MediaSources"],
	})
}

// handleStream 透传飞牛的 media/range 字节流给 Emby 客户端。
// 若飞牛走云盘/STRM 模式（stream 返回 cloud_storage_info 非空），
// 则直连云盘 URL（带 cookie/UA，115 限速），否则回退 media/range 本地透传。
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	versionID := r.URL.Query().Get("Version")
	if versionID == "" {
		versionID = r.URL.Query().Get("MediaSourceId")
	}
	info, err := h.fnOf(r.Context()).PlayInfoByGuid(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	mediaGuid := info.MediaGuid
	if mediaGuid == "" {
		mediaGuid = info.Guid
	}
	if versionID != "" && versionID != id {
		mediaGuid = versionID
	}

	// 1) 尝试云盘直链
	streamResp, err := h.fnOf(r.Context()).Stream(r.Context(), mediaGuid, "")
	if err == nil {
		direct := h.fnOf(r.Context()).ExtractDirectLink(streamResp)
		if direct.CloudType > 0 && direct.URL != "" {
			// 按 cloud_type 分流：115 限速 / 夸克分块 / 其它透明代理（serveCloudDirect 内部处理）
			h.serveCloudDirect(w, r, direct)
			return
		}
	}

	// 2) 本地 NAS 文件：media/range 透传
	// fork: 上游 media/range 一律返回 application/octet-stream，这里用文件名
	// 扩展名推导真实 MIME 以便纠正（部分播放器不看内容嗅探、只认 MIME）。
	mimeHint := ""
	if streamResp != nil && streamResp.FileStream.FileName != "" {
		mimeHint = filepath.Ext(streamResp.FileStream.FileName)
	}
	h.streamLocal(w, r, mediaGuid, mimeHint)
}

// streamHead 处理取流端点的 HEAD 请求。
//
// fork 新增：上游实现里 GET 路由会一并匹配 HEAD，HEAD 不带 Range 时
// RangeMedia 走完整 GET，于是整个文件（实测 6.7GB）被当作响应体往外推，
// 而 HEAD 并不发送响应体 —— 请求永不返回，客户端超时并判定「无法播放」。
// 这里改为向飞牛只请求 1 字节（bytes=0-0）拿头部与总长度，然后直接返回。
func (h *Handler) streamHead(w http.ResponseWriter, r *http.Request, mediaGuid, mimeHint, clientRange string) {
	hdr, _, body, err := h.fnOf(r.Context()).RangeMedia(r.Context(), mediaGuid, "bytes=0-0")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	body.Close()

	total := parseTotalSize(hdr.Get("Content-Range"))

	ct := hdr.Get("Content-Type")
	if genericMime(ct) {
		if m := mimeForExt(mimeHint); m != "" {
			ct = m
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if total > 0 {
		if clientRange != "" {
			if start, end, ok := parseClientRange(clientRange, total); ok {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
				w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
				w.WriteHeader(http.StatusPartialContent)
				return
			}
		}
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	}
	w.WriteHeader(http.StatusOK)
}

// streamLocal 本地 NAS 文件：media/range 透传。
// mimeHint 为按容器扩展名推导的 MIME（可为空），上游返回通用类型时用它纠正。
// 兜底：若客户端带 Range 而上游忽略返回 200 全量（带 Content-Length），
// 桥接自行裁剪回 206，保证官方客户端断点续传可用。
func (h *Handler) streamLocal(w http.ResponseWriter, r *http.Request, mediaGuid, mimeHint string) {
	rangeHdr := r.Header.Get("Range")

	// fork: HEAD 只取头部，绝不传体（见 streamHead 注释）
	if r.Method == http.MethodHead {
		h.streamHead(w, r, mediaGuid, mimeHint, rangeHdr)
		return
	}

	hdr, status, body, err := h.fnOf(r.Context()).RangeMedia(r.Context(), mediaGuid, rangeHdr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()

	// 上游无视 Range 回 200 全量：自己裁剪
	if rangeHdr != "" && status == http.StatusOK {
		if cl := hdr.Get("Content-Length"); cl != "" {
			var size int64
			if _, err := fmt.Sscanf(cl, "%d", &size); err == nil && size > 0 {
				if start, end, ok := parseClientRange(rangeHdr, size); ok && start < size {
					if _, err := io.CopyN(io.Discard, body, start); err == nil {
						ct := hdr.Get("Content-Type")
						if genericMime(ct) {
							if m := mimeForExt(mimeHint); m != "" {
								ct = m
							}
						}
						w.Header().Set("Content-Type", ct)
						w.Header().Set("Accept-Ranges", "bytes")
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
						w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
						w.Header().Set("Access-Control-Allow-Origin", "*")
						w.WriteHeader(http.StatusPartialContent)
						_, _ = io.CopyN(w, body, end-start+1)
						return
					}
				}
			}
		}
	}

	for _, k := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range"} {
		if v := hdr.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	// fork: 上游是通用 octet-stream 时，用容器扩展名推导的 MIME 覆盖
	if genericMime(w.Header().Get("Content-Type")) {
		if m := mimeForExt(mimeHint); m != "" {
			w.Header().Set("Content-Type", m)
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if status == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(status)
	copyStream(w, body)
}

// handleSessionsPlaying 官方客户端进度回传：POST /Sessions/Playing[/Progress]。
func (h *Handler) handleSessionsPlaying(w http.ResponseWriter, r *http.Request) {
	h.handleSessionsEvent(w, r, false)
}

// handleSessionsStopped 播放停止：位置接近结尾（≥92%）时标记飞牛已看完
// （对齐飞牛播放器行为：真机抓包播完自动调 item/watched）。
func (h *Handler) handleSessionsStopped(w http.ResponseWriter, r *http.Request) {
	h.handleSessionsEvent(w, r, true)
}

func (h *Handler) handleSessionsEvent(w http.ResponseWriter, r *http.Request, stopped bool) {
	var req struct {
		ItemId        string `json:"ItemId"`
		PositionTicks int64  `json:"PositionTicks"`
	}
	decodeJSON(r, &req)
	if req.ItemId == "" {
		req.ItemId = r.URL.Query().Get("ItemId")
	}
	h.recordToFN(r, req.ItemId, req.PositionTicks, stopped)
	w.WriteHeader(http.StatusNoContent)
}

// recordToFN 把位置（100ns ticks）转秒后回传飞牛，并记录桥接侧最后播放时间
// （飞牛 item/list 无最后播放时间字段——watched_ts 对未看完条目存的是位置秒——
// 继续观看排序只能靠自己记录）。
func (h *Handler) recordToFN(r *http.Request, itemID string, ticks int64, stopped bool) {
	if itemID == "" {
		return
	}
	sec := ticks / 10_000_000
	if info, err := h.fnOf(r.Context()).PlayInfoByGuid(r.Context(), itemID); err == nil {
		dur := playInfoSeconds(info)
		if err := h.fnOf(r.Context()).RecordProgress(r.Context(), itemID, info.MediaGuid, sec, dur); err == nil {
			touchProgress(itemID)
			// 进度刚上报就失效继续观看缓存：下次拉首页立即看到最新条目
			resumeMu.Lock()
			resumeAt = time.Time{}
			resumeMu.Unlock()
		}
		// 停止时位置接近结尾（≥92%）：标记已看完（飞牛 item/watched）
		if stopped && dur > 0 && sec >= dur*92/100 {
			if err := h.fnOf(r.Context()).WatchItem(r.Context(), itemID); err == nil {
				log.Printf("[watched] %s 已看完（%.0f%%，%ds/%ds）", itemID, float64(sec)/float64(dur)*100, sec, dur)
			}
		}
	}
}

// 桥接侧播放时间表：guid → 最后上报时刻（unix 秒）。落盘持久化，重启保留。
var (
	progressMu     sync.Mutex
	progressTimes  = map[string]int64{}
	progressLoaded bool
)

// progressPath 落盘位置：二进制工作目录下（与 bridge-config.json 同目录）。
func progressPath() string {
	return "bridge-progress.json"
}

func loadProgressLocked() {
	if progressLoaded {
		return
	}
	progressLoaded = true
	b, err := os.ReadFile(progressPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &progressTimes)
}

// touchProgress 记录某条目的最后播放时刻并落盘。
func touchProgress(guid string) {
	progressMu.Lock()
	defer progressMu.Unlock()
	loadProgressLocked()
	progressTimes[guid] = time.Now().Unix()
	b, err := json.Marshal(progressTimes)
	if err == nil {
		_ = os.WriteFile(progressPath(), b, 0o644)
	}
}

// lastPlayedAt 查条目的桥接侧最后播放时刻（无记录返回 0）。
func lastPlayedAt(guid string) int64 {
	progressMu.Lock()
	defer progressMu.Unlock()
	loadProgressLocked()
	return progressTimes[guid]
}

// handleProgress 老端点进度回传。
func (h *Handler) handleProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		PositionTicks int64 `json:"PositionTicks"`
	}
	decodeJSON(r, &req)
	h.recordToFN(r, id, req.PositionTicks, false)
	w.WriteHeader(http.StatusNoContent)
}

// handleItemImage 实现 GET /Items/{id}/Images/{type}[/{index}]：Emby 标准取图路径。
// 客户端会请求任意 ImageType（Primary/Logo/Thumb/Backdrop/Banner/Art...）；
// 飞牛只有海报资源：Primary 用竖版海报，其余类型优先取宽>高的横版海报（更接近
// Logo/Backdrop 用途），都没有时回退同一张海报。0.9.8 真机契约：海报走
// GET {base}/v/api/v1/sys/img{poster_path}?w=400，只需 Cookie（无需 authx）。
// 任何失败都回 1x1 透明 PNG（200），避免客户端破图。
func (h *Handler) handleItemImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// admin 后台自定义库图优先：命中直接 302（自定义 URL），不查飞牛
	if h.libImage != nil {
		if u := h.libImage(id); u != "" {
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
	}
	servePNG := func() {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png1x1())
	}
	// 人物头像（无状态）：People.PrimaryImageTag 是 base64url(飞牛 profile_path)，
	// 解码命中（以 / 开头）直接代理 sys/img——人物 guid 非条目，查 ItemDetail 必 404。
	// 普通海报 tag 为 "Primary"/"Logo" 等固定词（非合法长 base64，不会误命中）。
	if tag := queryGet(r.URL.Query(), "tag"); len(tag) >= 8 {
		if b, err := base64.URLEncoding.DecodeString(tag); err == nil && len(b) > 0 && b[0] == '/' {
			h.serveSysImg(w, r, string(b))
			return
		}
	}
	item, err := h.fnOf(r.Context()).ItemDetail(r.Context(), id)
	if err != nil {
		log.Printf("[poster] item %q detail: %v", id, err)
		servePNG()
		return
	}
	// 飞牛只有竖版海报资源（item posters 为单值字符串）：
	// Logo/Thumb/Backdrop 等类型回退同一张海报（横版位裁剪显示，好过透明图）
	p := item.PosterPath()
	// 按请求类型取对应资源（0.9.8 真机契约：剧有 logos/backdrops，集 poster 即横版剧照）
	switch strings.ToLower(r.PathValue("imgType")) {
	case "logo":
		p = item.LogoPath()
		// 季/集无自有 Logo：沿 parent 链向上找（集→季→剧），取第一个有 logos 的
		// （真机契约：季 parent_guid=剧，集 parent_guid=季；ancestor_guid 指库不可用）
		if p == "" && (item.Type == "Season" || item.Type == "Episode") {
			g := item.ParentGuid
			for i := 0; i < 2 && g != "" && g != item.Guid; i++ {
				det, err := h.fnOf(r.Context()).ItemDetail(r.Context(), g)
				if err != nil {
					break
				}
				if det.LogoPath() != "" {
					p = det.LogoPath()
					break
				}
				g = det.ParentGuid
			}
		}
	case "backdrop", "thumb", "art", "banner":
		if v := item.BackdropPath(); v != "" {
			p = v
		}
	}
	if p == "" && (item.Type == "MediaDB" || item.Type == "Directory" || item.Type == "TV") {
		// 库/剧等容器自身无海报：回退库内第一个有海报的作品
		// （首页"我的媒体"库卡片空白图修复）
		if list, err := h.fnOf(r.Context()).ItemList(r.Context(), id); err == nil {
			for _, it := range list { // 优先剧/电影
				if it.Type != "TV" && it.Type != "Movie" {
					continue
				}
				if pp := it.PosterPath(); pp != "" {
					p = pp
					break
				}
			}
			if p == "" {
				for _, it := range list {
					if pp := it.PosterPath(); pp != "" {
						p = pp
						break
					}
				}
			}
		}
	}
	if p == "" {
		log.Printf("[poster] item %q 无海报路径", id)
		servePNG()
		return
	}
	h.serveSysImg(w, r, p)
}

// serveSysImg 代理飞牛 sys/img 图片资源（海报/头像/Logo 统一出口）。
// 绝对 URL（云盘封面等）302 让客户端直连。
func (h *Handler) serveSysImg(w http.ResponseWriter, r *http.Request, p string) {
	servePNG := func() {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png1x1())
	}
	// 绝对 URL（云盘封面等）：302 让客户端直连
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		http.Redirect(w, r, p, http.StatusFound)
		return
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// 分辨率：透传客户端请求的 maxWidth/maxHeight（此前写死 w=400，
	// 高分屏拉伸显示发糊——飞牛影视 App 直连原图不糊）。真机实测 sys/img
	// 全档位生效（w=200..3000，不传给原图）。宁大勿小保证清晰，上限 3000。
	wParam := 800
	if v := queryGet(r.URL.Query(), "maxWidth"); v != "" {
		if n := parseInt(v); n > 0 {
			wParam = n
		}
	} else if v := queryGet(r.URL.Query(), "maxHeight"); v != "" {
		if n := parseInt(v); n > wParam {
			wParam = n
		}
	}
	if wParam > 3000 {
		wParam = 3000
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		h.fnOf(r.Context()).BaseURL+"/v/api/v1/sys/img"+p+"?w="+strconv.Itoa(wParam), nil)
	if err != nil {
		servePNG()
		return
	}
	req.Header.Set("User-Agent", h.fnOf(r.Context()).UserAgent)
	for k, v := range h.fnOf(r.Context()).AuthHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := h.fnOf(r.Context()).DoRaw(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 120))
			resp.Body.Close()
			log.Printf("[poster] sys/img %q: err=%v status=%d body=%s", p, err, resp.StatusCode, string(b))
		} else {
			log.Printf("[poster] sys/img %q: err=%v", p, err)
		}
		servePNG()
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(http.StatusOK)
	copyStream(w, resp.Body)
}

// ---- 转换 ----

// imageTags 生成 BaseItemDto.ImageTags：客户端只在 tag 非空时才发对应图片请求
// （此前只填 Primary，Logo/Thumb 请求根本不会发出——Yamby "logo 没有了" 的根因）。
// Primary=海报；Logo 用飞牛 logos 资源；Thumb/Backdrop/Art/Banner 用 backdrops
// （集 poster 本身即 1920×1080 横版剧照，回退海报裁剪显示）。
func imageTags(m fn.MediaItem) map[string]any {
	tags := map[string]any{"Primary": "Primary"}
	// Logo：剧用自有 logos；季/集无自有资源但 handleItemImage 会回退所属剧的
	// logos，同样声明（客户端只有看到 tag 才会发请求——季/集详情页标题图）
	if m.LogoPath() != "" || m.Type == "Season" || m.Type == "Episode" {
		tags["Logo"] = "Logo"
	}
	if m.BackdropPath() != "" || m.PosterPath() != "" {
		tags["Thumb"] = "Thumb"
		tags["Backdrop"] = "Backdrop"
		tags["Art"] = "Art"
		tags["Banner"] = "Banner"
	}
	return tags
}

// toEmbyItemFromPlayInfo 把飞牛 PlayInfo 转成 Emby Item 结构。
// 海报走标准 /Items/{id}/Images/Primary 端点（见 handleImagesPrimary），
// 这里只需在 ImageTags.Primary 放一个非空 tag 让客户端来取。
// 字段对照 Emby 官方 BaseItemDto（betadev.emby.media/reference/RestAPI）：
// IndexNumber=集号, ParentIndexNumber=季号, SeriesName/SeriesId/SeasonId=剧集归属。
func toEmbyItemFromPlayInfo(info *fn.PlayInfo, h *Handler) map[string]any {
	itemType := "Movie"
	switch info.Type {
	case "Episode":
		itemType = "Episode"
	case "Video":
		itemType = "Video"
	}
	item := map[string]any{
		"Id":                      info.Guid,
		"ServerId":                h.ServerID,
		"Name":                    displayName(info),
		"Type":                    itemType,
		"SortName":                info.Item.Title,
		"ParentId":                info.ParentGuid,
		"PrimaryImageAspectRatio": 0.6667,
		"Path":                    "fn://" + info.Guid,
		"RunTimeTicks":            playInfoSeconds(info) * 10_000_000,
		"CanDelete":               false,
		"IsFolder":                false,
		"Container":               "mp4,mkv",
		"MediaSourceId":           info.Guid,
		"MediaSources": []map[string]any{{
			"Id":                   info.Guid,
			"Path":                 "fn://" + info.Guid,
			"Protocol":             "Http",
			"Container":            "mp4,mkv",
			"Type":                 "Virtual",
			"SupportsDirectPlay":   true,
			"SupportsDirectStream": true,
			"SupportsTranscoding":  false,
			"MediaStreams":         []map[string]any{}, // MediaSourceInfo 必填（Yamby 严格序列化）
		}},
		"ImageTags":         map[string]any{"Primary": "Primary"}, // play/info 无 logos/backdrops，播放场景仅 Primary
		"IndexNumber":       info.Item.EpisodeNum,                 // BaseItemDto.IndexNumber: 集号
		"ParentIndexNumber": info.Item.SeasonNum,                  // BaseItemDto.ParentIndexNumber: 季号
		"SeasonName":        fmt.Sprintf("第 %d 季", info.Item.SeasonNum),
		"SeriesName":        info.Item.TvTitle, // BaseItemDto.SeriesName
		"SeriesId":          info.GrandGuid,    // 剧 guid
		"SeasonId":          info.ParentGuid,   // 季 guid
		"Overview":          info.Item.Overview,
		"PremiereDate":      nullableDate(normalizeEmbyDate(info.Item.AirDate)),
		"ProductionYear":    yearOf(info.Item.AirDate),
		// kotlinx.serialization 强类型补齐（同 toEmbyItemWithSeries 的说明）
		"CanDownload":         false,
		"OriginalTitle":       displayName(info),
		"OfficialRating":      "",
		"CriticRating":        0,
		"Status":              "",
		"EndDate":             nil, // 官方无值给 null；空串会让 Dart DateTime.parse("") 抛 FormatException
		"DateCreated":         nullableDate(normalizeEmbyDate(info.Item.AirDate)),
		"Genres":              []any{},
		"GenreItems":          []any{},
		"Studios":             []any{},
		"Taglines":            []any{},
		"Tags":                []any{},
		"ProductionLocations": []any{},
		"AirDays":             []any{},
		"AirTime":             "",
		"BackdropImageTags":   []any{},
		"Chapters":            []any{},
		"Width":               0,
		"Height":              0,
		"Size":                0,
		"Etag":                "",
		"LocationType":        "FileSystem",
		"UserData": map[string]any{
			"Key":                   info.Guid,
			"Played":                info.Item.IsWatched != 0,
			"IsFavorite":            false,
			"PlaybackPositionTicks": info.Ts * 10_000_000, // 续播位置（秒→ticks）
		},
	}
	fillItemCommon(item)
	for _, msrc := range item["MediaSources"].([]map[string]any) {
		fillMediaSource(msrc)
	}
	return item
}

// normalizeEmbyDate 把飞牛的纯日期（"2019-01-07"）规范化为 Emby 官方
// ISO round-trip 格式（"2019-01-07T00:00:00.0000000Z"）。Yamby 等强类型
// 客户端的日期解析器按完整 ISO8601 pattern 解析，纯日期字符串直接抛异常，
// 表现为「请求 200 但详情/集列表渲染中断、无后续请求」。已是完整格式则原样。
func normalizeEmbyDate(s string) string {
	if s == "" {
		return ""
	}
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02T15:04:05Z07:00", // RFC3339（统一到 7 位小数 + Z 形态）
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.0000000Z")
		}
	}
	return s
}

func yearOf(dates ...string) int {
	for _, d := range dates {
		if len(d) >= 4 {
			y := 0
			if _, err := fmt.Sscanf(d[:4], "%d", &y); err == nil && y > 0 {
				return y
			}
		}
	}
	return 0
}

// toEmbyItems 批量把 MediaItem 转成 Emby Item。
func toEmbyItems(r *http.Request, items []fn.MediaItem, h *Handler) []map[string]any {
	return toEmbyItemsWithSeries(r, items, "", h)
}

// toEmbyItemsWithSeries 同上，但为集条目补充 SeriesId（剧 guid 仅下钻场景可知，
// 列表平铺时飞牛集条目不含剧 guid——ancestor_guid 指向库）。
func toEmbyItemsWithSeries(r *http.Request, items []fn.MediaItem, seriesID string, h *Handler) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		out = append(out, toEmbyItemWithSeries(r, items[i], seriesID, h))
	}
	return out
}

func toEmbyItemWithSeries(r *http.Request, m fn.MediaItem, seriesID string, h *Handler) map[string]any {
	// 飞牛 type 直接映射 Emby Type；TV/Season/Directory 是容器（可下钻）
	itemType := "Movie"
	isFolder := false
	switch m.Type {
	case "TV":
		itemType, isFolder = "Series", true
	case "Season":
		itemType, isFolder = "Season", true
	case "Episode":
		itemType = "Episode"
	case "Directory":
		itemType, isFolder = "Folder", true
	case "MediaDB": // 媒体库自身（item/{库guid} 返回 type=MediaDB）
		itemType, isFolder = "Folder", true
	case "Video":
		itemType = "Video"
	}
	name := m.Title
	if name == "" && m.TvTitle != "" {
		name = fmt.Sprintf("%s S%02dE%02d", m.TvTitle, m.SeasonNumber, m.EpisodeNumber)
	}
	// 时长（秒）：duration 优先，回退 runtime（分钟）×60
	dur := int64(m.Duration)
	if dur <= 0 {
		dur = int64(m.Runtime) * 60
	}
	// 首播日期多字段兜底（官方 BaseItemDto.PremiereDate；飞牛 TV 剧集在 first_air_date）
	// → 规范化为官方 ISO round-trip 格式（Yamby 强类型日期解析要求完整 ISO8601）
	premiere := normalizeEmbyDate(m.AirDate)
	if premiere == "" {
		premiere = normalizeEmbyDate(m.ReleaseDate)
	}
	if premiere == "" {
		premiere = normalizeEmbyDate(m.FirstAirDate)
	}
	// DateCreated（入库时间）：web 客户端详情页 renderMediaSources 会
	// new Date(Date.parse(item.DateCreated)) 后 Intl 格式化，缺失时得到
	// Invalid Date → "RangeError: Invalid time value" 整页渲染中断。
	// 飞牛无入库时间字段，用首播日期兜底，再兜当前时间。
	dateCreated := premiere
	if dateCreated == "" {
		dateCreated = time.Now().UTC().Format(time.RFC3339)
	}
	item := map[string]any{
		"Id":                      m.Guid,
		"ServerId":                h.ServerID,
		"Name":                    name,
		"Type":                    itemType,
		"IsFolder":                isFolder,
		"SortName":                m.Title,
		"ParentId":                m.ParentGuid,
		"PrimaryImageAspectRatio": 0.6667,
		"Path":                    "fn://" + m.Guid,
		"RunTimeTicks":            dur * 10_000_000,
		"CanDelete":               false,
		"Container":               "mp4,mkv",
		"MediaSourceId":           m.Guid,
		"Overview":                m.Overview,
		"PremiereDate":            nullableDate(premiere),
		"DateCreated":             nullableDate(dateCreated),
		"ProductionYear":          yearOf(m.AirDate, m.ReleaseDate, m.FirstAirDate),
		"ImageTags":               imageTags(m),
	}
	// 通用空集合字段：Flutter 客户端对列表字段常用 ! 强解包，缺失即
	// "Null check operator used on a null value"（官方对无数据的列表也返回空数组）
	item["Genres"] = []any{}
	item["GenreItems"] = []any{}
	item["Studios"] = []any{}
	item["Taglines"] = []any{}
	item["BackdropImageTags"] = []any{}
	item["ScreenshotImageTags"] = []any{}
	item["Chapters"] = []any{}
	item["LocationType"] = "FileSystem"
	// 官方 BaseItemDto 常规字段补齐：Yamby 用 kotlinx.serialization（强类型），
	// serializer 描述符里出现过的字段（dex strings 确认 Status/EndDate/
	// OriginalTitle/CanDownload/CriticRating/Width/Height/Size 等）若为
	// 非空无默认值属性，响应缺失即 MissingFieldException → 详情页静默崩
	// （表现：请求全 200 但页面不渲染、无后续请求——「点进剧集进不去」）。
	// 全部给安全空值（官方对无数据场景也返回 ""/0/[]）。
	item["CanDownload"] = false
	item["OriginalTitle"] = name
	item["OfficialRating"] = ""
	item["CriticRating"] = 0
	item["Status"] = ""   // Series 分支有值时覆盖（Continuing/Ended）
	item["EndDate"] = nil // 空串会让 Dart DateTime.parse("") 抛 FormatException
	item["Tags"] = []any{}
	item["ProductionLocations"] = []any{}
	item["AirDays"] = []any{}
	item["AirTime"] = ""
	item["Width"] = 0
	item["Height"] = 0
	item["Size"] = 0
	item["Etag"] = ""
	item["Album"] = ""
	item["AlbumArtists"] = []any{}
	item["ArtistItems"] = []any{}
	// ProviderIds/ExternalUrls：飞牛刮削的 IMDb ID（客户端"外部链接"入口，
	// Yamby 列表/详情请求都带 Fields=ProviderIds,ExternalUrls）
	if m.ImdbID != "" {
		item["ProviderIds"] = map[string]any{"Imdb": m.ImdbID}
		item["ExternalUrls"] = []any{map[string]any{
			"Name": "IMDb", "Url": "https://www.imdb.com/title/" + m.ImdbID,
		}}
	}
	// MediaSources：仅可播条目携带（官方容器类型 Series/Season/Folder 不返回），
	// 且 MediaSourceInfo.MediaStreams 为必填（Yamby Kotlin 严格序列化，缺字段直接抛异常）。
	// Path 必须是真实播放 URL：客户端（Hills）会直接把列表条目里的
	// MediaSources.Path 交给播放器内核——fn:// 伪协议零请求立即失败
	// （"从继续观看卡片点播放没反应"根因）。
	switch itemType {
	case "Movie", "Episode", "Video":
		playURL := ""
		if r != nil {
			playURL = h.streamURL(r, m.Guid)
		}
		// MediaType：官方对可播视频类型返回 "Video"。缺了它 web 客户端
		// canPlayerPlayMediaType(player, undefined) 会过滤掉全部本地播放器，
		// 集详情页 getPlayer(...) 返回 undefined → "reading 'getDeviceProfile'" 崩
		item["MediaType"] = "Video"
		item["Path"] = playURL
		item["MediaSources"] = []map[string]any{{
			"Id":                   m.Guid,
			"Path":                 playURL,
			"DirectStreamUrl":      playURL,
			"Protocol":             "Http",
			"Container":            "mp4,mkv",
			"Type":                 "Virtual",
			"SupportsDirectPlay":   true,
			"SupportsDirectStream": true,
			"SupportsTranscoding":  false,
			"MediaStreams":         []map[string]any{},
		}}
		for _, msrc := range item["MediaSources"].([]map[string]any) {
			fillMediaSource(msrc)
		}
	}
	// 类型专属字段（真机 0.9.8 数据确认；字段名对照官方 BaseItemDto）：
	switch itemType {
	case "Episode":
		// IndexNumber=集号, ParentIndexNumber=季号（Yamby 集角标 S?E12 即因缺后者）
		item["IndexNumber"] = m.EpisodeNumber
		item["ParentIndexNumber"] = m.SeasonNumber
		item["SeasonName"] = m.ParentTitle // 真机为"第 2 季"
		item["SeriesName"] = m.TvTitle
		item["SeasonId"] = m.ParentGuid // 集的 parent 即季
		if seriesID != "" {
			item["SeriesId"] = seriesID // 剧 guid 仅下钻场景可知（ancestor_guid 是库）
			// Yamby/Flutter 集详情页的剧 logo/海报直接读这些字段（null 即
			// "Null check operator" 崩 + 无 logo）：
			item["ParentLogoItemId"] = seriesID
			item["ParentLogoImageTag"] = "Logo"
			item["ParentPrimaryImageItemId"] = seriesID
			item["ParentPrimaryImageTag"] = "Primary"
			item["ParentThumbItemId"] = m.ParentGuid // 季横版
			item["ParentThumbImageTag"] = "Thumb"
			item["SeriesPrimaryImageTag"] = "Primary"
			item["SeriesThumbImageTag"] = "Thumb"
		}
	case "Season":
		// 季：IndexNumber=季号；SeriesId=季的 parent（真机确认指向剧 guid）
		item["IndexNumber"] = m.SeasonNumber
		item["SeriesId"] = m.ParentGuid
		if cc := m.NumberOfEpisodes; cc > 0 {
			item["ChildCount"] = cc
		} else if cc := m.EpisodeNumber; cc > 0 { // 季条目 episode_number=本季集数
			item["ChildCount"] = cc
		} else if cc := m.LocalNumberOfEpisodes; cc > 0 {
			item["ChildCount"] = cc
		}
	case "Series":
		if n := m.NumberOfSeasons; n > 0 {
			item["ChildCount"] = n  // 季数
			item["SeasonCount"] = n // BaseItemDto.SeasonCount
		}
		if n := m.NumberOfEpisodes; n > 0 {
			item["RecursiveItemCount"] = n // 总集数
		}
		switch m.Status {
		case "Returning Series":
			item["Status"] = "Continuing" // Emby 枚举：Continuing/Ended
		case "Ended", "Canceled":
			item["Status"] = m.Status
		}
	}
	if v := voteOf(m.VoteAverage); v > 0 {
		item["CommunityRating"] = v
	}
	// UserData（官方 UserItemDataDto）：Played/PlayCount/续播位置/收藏——
	// 飞牛 ts=续播位置秒，watched=1 已看完
	played := m.Watched != 0 || m.IsWatched != 0
	ud := map[string]any{
		"Key":                   m.Guid,
		"Played":                played,
		"IsFavorite":            m.IsFavorite != 0,
		"PlaybackPositionTicks": m.Ts * 10_000_000,
	}
	if played {
		ud["PlayCount"] = 1
	}
	// 总时长 + 进度百分比：客户端"继续观看"卡片进度条靠 PlayedPercentage
	// （只有位置没有总时长时只能显示"已播放至 x 秒"，拖动条比例算不出）。
	// 飞牛集条目 duration=秒、剧/季 runtime=分钟；此项同时补条目 RunTimeTicks。
	total := int64(m.Duration)
	if total == 0 && m.Runtime > 0 {
		total = int64(m.Runtime) * 60
	}
	if total > 0 {
		item["RunTimeTicks"] = total * 10_000_000
		if m.Ts > 0 && !played {
			ud["PlayedPercentage"] = float64(m.Ts) / float64(total) * 100
		}
	}
	// LastPlayedDate：watched_ts 对未看完条目存的是位置秒（会输出 1970-01-01 假时间），
	// 只有像合理 Unix 时间戳（>2001 年）才直接用；否则用桥接侧自己记录的播放时刻
	ts := m.WatchedTs
	if ts <= 1_000_000_000 {
		ts = lastPlayedAt(m.Guid)
	}
	if ts > 1_000_000_000 {
		ud["LastPlayedDate"] = time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05.0000000Z")
	}
	item["UserData"] = ud
	fillItemCommon(item)
	return item
}

// voteOf 把飞牛 vote_average（string 评分）转 float，容错非法值。
func voteOf(s string) float64 {
	if s == "" {
		return 0
	}
	v := 0.0
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return 0
	}
	return v
}

func displayName(info *fn.PlayInfo) string {
	if info.Item.Title != "" {
		return info.Item.Title
	}
	if info.Item.TvTitle != "" {
		return fmt.Sprintf("%s S%02dE%02d", info.Item.TvTitle, info.Item.SeasonNum, info.Item.EpisodeNum)
	}
	return info.Guid
}

// handleNextUp 实现 GET /Shows/NextUp：剧详情"继续播放"横幅的数据源。
// 官方语义"下一集"：优先取第一个"有进度且未看完"的集（=继续播放目标），
// 全没看过取第一集，全看完返回空。之前恒返回空 → Hills 继续播放区块
// 拿不到条目（总时长/进度显示缺失，"继续播放那里没有总时长"）。
func (h *Handler) handleNextUp(w http.ResponseWriter, r *http.Request) {
	empty := func() {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
	}
	seriesID := queryGet(r.URL.Query(), "SeriesId")
	if seriesID == "" {
		seriesID = queryGet(r.URL.Query(), "ParentId")
	}
	if seriesID == "" {
		empty() // 首页级 NextUp（无剧 guid）暂不支持
		return
	}
	eps := h.seriesEpisodes(r.Context(), seriesID)
	var next *fn.MediaItem
	for i := range eps {
		if eps[i].Ts > 0 && eps[i].Watched == 0 && eps[i].IsWatched == 0 {
			next = &eps[i]
			break
		}
	}
	if next == nil {
		for i := range eps {
			if eps[i].Watched == 0 && eps[i].IsWatched == 0 {
				next = &eps[i]
				break
			}
		}
	}
	if next == nil {
		empty()
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Items":            []map[string]any{toEmbyItemWithSeries(r, *next, seriesID, h)},
		"TotalRecordCount": 1,
		"StartIndex":       0,
	})
}

// handleShowsSeasons 实现 GET /Shows/{id}/Seasons：官方 App 剧集季导航。
func (h *Handler) handleShowsSeasons(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 优先飞牛 season/list，失败/为空回退 item/list（parent 模式）
	seasons, err := h.fnOf(r.Context()).SeasonList(r.Context(), id)
	if err != nil || len(seasons) == 0 {
		seasons, _ = h.fnOf(r.Context()).ItemList(r.Context(), id)
	}
	sort.Slice(seasons, func(i, j int) bool { return seasons[i].SeasonNumber < seasons[j].SeasonNumber })
	writeJSON(w, http.StatusOK, map[string]any{
		"Items":            toEmbyItemsWithSeries(r, seasons, id, h),
		"TotalRecordCount": len(seasons),
		"StartIndex":       0,
	})
}

// handleShowsEpisodes 实现 GET /Shows/{id}/Episodes?SeasonId=：官方 App 集导航。
// 无 SeasonId 时平铺该剧全部集（飞牛 item/list 对剧 guid 返回季列表，需逐季聚合）。
// 剧 guid 从路径取，供集条目 SeriesId 映射。
func (h *Handler) handleShowsEpisodes(w http.ResponseWriter, r *http.Request) {
	seriesID := r.PathValue("id")
	// Emby 服务器 query 参数大小写不敏感，客户端可能传 SeasonId/seasonId
	guid := queryGet(r.URL.Query(), "SeasonId")
	if guid == "" {
		guid = seriesID
	}
	eps, err := h.fnOf(r.Context()).ItemList(r.Context(), guid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"Error": err.Error(), "Items": []any{}})
		return
	}
	// 无 SeasonId（或传入剧 guid）：item/list 返回的是季 → 逐季聚合平铺全部集
	if guid == seriesID && len(eps) > 0 && eps[0].Type == "Season" {
		sort.Slice(eps, func(i, j int) bool { return eps[i].SeasonNumber < eps[j].SeasonNumber })
		var all []fn.MediaItem
		for _, s := range eps {
			el, err := h.fnOf(r.Context()).ItemList(r.Context(), s.Guid)
			if err != nil {
				continue
			}
			all = append(all, el...)
		}
		eps = all
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].EpisodeNumber < eps[j].EpisodeNumber })
	writeJSON(w, http.StatusOK, map[string]any{
		"Items":            toEmbyItemsWithSeries(r, eps, seriesID, h),
		"TotalRecordCount": len(eps),
		"StartIndex":       0,
	})
}

// handleEmptyList 通用空列表响应（/Items/{id}/Intros 等客户端可选端点）。
func (h *Handler) handleEmptyList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
}

// handleVideosSub 兜住 /Videos/{id}/ 后带任意后缀的流路径
// （官方 web 客户端请求 stream.mp4,mkv,mov,webm,avi,ts——容器列表拼在段里）。
func (h *Handler) handleVideosSub(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if strings.HasPrefix(strings.ToLower(rest), "stream") {
		h.handleStream(w, r)
		return
	}
	h.handleFallback(w, r)
}

// handleSubtitleStream 实现 GET /Videos/{id}/{msId}/Subtitles/{index}/Stream.{fmt}。
// 飞牛无字幕内容下发接口（stream/subtitle 等路径实测返回空），内嵌字幕由
// 客户端直连容器读取；此处显式 404，避免兜底 JSON 被客户端当字幕解析报错。
func (h *Handler) handleSubtitleStream(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "subtitle not available via bridge (embedded in container)", http.StatusNotFound)
}

// handleQueryFilters 实现 GET /Items/Filters 与 /Users/{uid}/Items/Filters。
//
// Emby 返回 QueryFilters（Genres / Tags / OfficialRatings / Years）。飞牛侧没有
// 这层筛选项数据，回空数组即可 —— **关键是端点必须存在**：此前该路径被
// /Items/{id} 通配吃掉，桥接拿 "Filters" 当作影片 ID 去查飞牛并返回 404，
// 而客户端打开媒体库时会调用它，404 会导致媒体库页打不开。
func (h *Handler) handleQueryFilters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"Genres":          []any{},
		"Tags":            []any{},
		"OfficialRatings": []any{},
		"Years":           []any{},
	})
}

// handleVirtualFolders 实现 GET /Library/VirtualFolders。
// Emby 返回 VirtualFolderInfo[]（**数组**，不是 QueryResult 对象）。
// CollectionType 与 /Users/{uid}/Views 保持一致：Other（其他视频）归混合内容，
// 即不声明该字段。
func (h *Handler) handleVirtualFolders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbs, err := h.fnOf(ctx).MediaDBList(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	out := make([]map[string]any, 0, len(dbs))
	for _, db := range dbs {
		f := map[string]any{
			"Name":           db.Title,
			"Locations":      []any{},
			"ItemId":         db.Guid,
			"LibraryOptions": map[string]any{"PathInfos": []any{}},
		}
		if ct := collectionType(db.Category); ct != "" {
			f["CollectionType"] = ct
		}
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleItemCountsAll 实现 GET /Items/Counts 与 /Users/{uid}/Items/Counts。
//
// Emby 的 ItemCounts 是一组计数字段的对象，不是 QueryResult —— 此前该路径
// 返回 {"Items":[],...}，字段全对不上。计数由各媒体库条目数汇总，
// 按库类别分别归入 MovieCount / SeriesCount。
func (h *Handler) handleItemCountsAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts := map[string]any{
		"MovieCount": 0, "SeriesCount": 0, "EpisodeCount": 0, "ArtistCount": 0,
		"ProgramCount": 0, "TrailerCount": 0, "SongCount": 0, "AlbumCount": 0,
		"BoxSetCount": 0, "BookCount": 0, "ItemCount": 0,
	}
	if dbs, err := h.fnOf(ctx).MediaDBList(ctx); err == nil {
		total, movies, series := 0, 0, 0
		for _, db := range dbs {
			_, n, err := h.fnOf(ctx).ItemListPage(ctx, "ancestor_guid", db.Guid, 1, 1)
			if err != nil || n <= 0 {
				continue
			}
			total += n
			switch collectionType(db.Category) {
			case "movies":
				movies += n
			case "tvshows":
				series += n
			}
		}
		counts["ItemCount"] = total
		counts["MovieCount"] = movies
		counts["SeriesCount"] = series
	}
	writeJSON(w, http.StatusOK, counts)
}

// handleItemCounts 实现 GET /Items/{id}/Counts：客户端仪表盘统计。
func (h *Handler) handleItemCounts(w http.ResponseWriter, r *http.Request) {
	dbs, err := h.fnOf(r.Context()).MediaDBList(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"LibraryCount": len(dbs),
		"ServerId":     h.ServerID,
	})
}

// handleEmptyArray 返回纯 JSON 数组（/Items/{id}/SpecialFeatures 等，
// web 客户端直接 .slice/.length，给 QueryResult 对象会崩）。
func (h *Handler) handleEmptyArray(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// handleSessionCapabilities 官方 SessionCapabilitiesDto（客户端登录后拉取，
// 服务器据此下发可用命令集）。
func (h *Handler) handleSessionCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"PlayableMediaTypes":         []string{"Video"},
		"SupportedCommands":          []string{"DisplayMessage", "Play"},
		"SupportsMediaControl":       false,
		"SupportsPersistentTimeline": false,
	})
}

// handleSessionCapabilitiesPost 客户端上报自身能力（静默成功）。
func (h *Handler) handleSessionCapabilitiesPost(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleThemeMedia 实现 GET /Items/{id}/ThemeMedia：主题音乐/主题视频。
// web 客户端 thememediaplayer 直接读 themeMediaResult.ThemeVideosResult.Items.length，
// 兜底返回的 {} 会让详情页抛 "reading 'Items'"，必须返回标准 QueryResult 空壳。
func (h *Handler) handleThemeMedia(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ThemeSongsResult":  map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0},
		"ThemeVideosResult": map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0},
	})
}

// handleLatest 实现 GET /Users/{uid}/Items/Latest：首页"最新添加"（返回数组格式）。
// Yamby 真机契约：首页每个媒体库区块各发一次请求，带 ParentId=库 guid——
// 必须只返回该库的条目（此前忽略 ParentId 导致所有区块显示同一份跨库混合数据）。
// 不带 ParentId 时（全局"最新添加"）才做跨库聚合。
func (h *Handler) handleLatest(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r.URL.Query(), "Limit", 20)
	if limit <= 0 {
		limit = 20
	}
	if parent := r.URL.Query().Get("ParentId"); parent != "" {
		list, err := h.fnOf(r.Context()).ItemListContainers(r.Context(), parent, limit)
		if err != nil {
			list = nil
		}
		writeJSON(w, http.StatusOK, toEmbyItemsOrEmpty(r, list, h))
		return
	}
	dbs, err := h.fnOf(r.Context()).MediaDBList(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	var all []fn.MediaItem
	for _, db := range dbs {
		// 服务端只取容器级（Movie/TV/Directory/Video）：Latest 展示"最新添加的剧/电影"，
		// 点进去才是季/集；客户端侧过滤在大库下会漏（前 N 页可能全是集）
		list, err := h.fnOf(r.Context()).ItemListContainers(r.Context(), db.Guid, limit)
		if err != nil {
			continue
		}
		all = append(all, list...)
		if len(all) >= limit {
			break
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	writeJSON(w, http.StatusOK, toEmbyItemsOrEmpty(r, all, h))
}

// toEmbyItemsOrEmpty toEmbyItems 的 nil 安全版。
func toEmbyItemsOrEmpty(r *http.Request, items []fn.MediaItem, h *Handler) []map[string]any {
	out := toEmbyItems(r, items, h)
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

// 继续观看快照缓存：Yamby 每次刷新首页都会请求 Resume，全库聚合有翻页成本，
// 缓存 60s（进度刚播完就上首页的延迟上限 1 分钟，可接受）。
var (
	resumeMu    sync.Mutex
	resumeItems []fn.MediaItem
	resumeAt    time.Time
)

// handleResume 实现 GET /Users/{uid}/Items/Resume：首页"继续观看"。
// 飞牛无现成列表 API（watch_list / play/record/list 等真机均 501），
// item/list 的 watched_ts/ts 排序也不生效（真机验证：DESC 排序结果仍是 create_time 序），
// 所以桥接侧自行聚合：全库拉集级条目（Episode/Movie/Video），筛"有进度且未看完"，
// 按最后播放时间（watched_ts，未看完时飞牛也写入最后播放时刻）倒序。
func (h *Handler) handleResume(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r.URL.Query(), "Limit", 20)
	if limit <= 0 {
		limit = 20
	}
	all := resumeSnapshot(r.Context(), h.FN)
	items := all
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Items":            toEmbyItemsOrEmpty(r, items, h),
		"TotalRecordCount": len(all),
		"StartIndex":       0,
	})
}

// resumeSnapshot 取全库"继续观看"候选（ts>0 且未看完），按最后播放时间倒序，60s 缓存。
func resumeSnapshot(ctx context.Context, c *fn.Client) []fn.MediaItem {
	resumeMu.Lock()
	defer resumeMu.Unlock()
	if resumeItems != nil && time.Since(resumeAt) < 60*time.Second {
		return resumeItems
	}
	var all []fn.MediaItem
	if dbs, err := c.MediaDBList(ctx); err == nil {
		for _, db := range dbs {
			list, err := c.ItemListTyped(ctx, db.Guid, []string{"Episode", "Movie", "Video"}, 0)
			if err != nil {
				continue
			}
			for _, m := range list {
				if m.Ts > 0 && m.Watched == 0 && m.IsWatched == 0 {
					all = append(all, m)
				}
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		// 桥接侧记录的播放时间优先（真实时间序）；都无记录时回退 watched_ts
		// （注意：watched_ts 对未看完条目是位置秒而非时间戳，只作弱排序兜底）
		ti, tj := lastPlayedAt(all[i].Guid), lastPlayedAt(all[j].Guid)
		if ti != tj {
			return ti > tj
		}
		return all[i].WatchedTs > all[j].WatchedTs
	})
	if all == nil {
		all = []fn.MediaItem{}
	}
	resumeItems, resumeAt = all, time.Now()
	return all
}

// 首页布局偏好：内存态即可（重启丢失无妨，客户端会重新 POST）
var displayPrefs sync.Map // key -> map[string]any

func (h *Handler) handleDisplayPrefsGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	prefs := map[string]any{
		"Id":              key,
		"UserId":          r.URL.Query().Get("userId"),
		"Client":          "emby",
		"Key":             key,
		"ViewType":        "",
		"CustomPrefs":     map[string]any{},
		"SortBy":          "SortName",
		"SortOrder":       "Ascending",
		"IndexBy":         nil,
		"RememberIndexes": false,
	}
	if v, ok := displayPrefs.Load(key); ok {
		if m, ok := v.(map[string]any); ok {
			prefs["CustomPrefs"] = m
		}
	}
	writeJSON(w, http.StatusOK, prefs)
}

func (h *Handler) handleDisplayPrefsPost(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req struct {
		CustomPrefs map[string]any `json:"CustomPrefs"`
	}
	decodeJSON(r, &req)
	if req.CustomPrefs != nil {
		displayPrefs.Store(key, req.CustomPrefs)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEmbySocket 实现 GET /embysocket：Emby 官方客户端登录后必连的 WebSocket。
// 缺失时部分客户端版本直接报"无法连接服务器"。实现最小协议：
// 握手成功后，回应客户端 KeepAlive 并周期发送服务端 KeepAlive。
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

const wsKeepAliveMsg = `{"MessageType":"KeepAlive","Data":{}}`

func (h *Handler) handleEmbySocket(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade 已写错误响应
	}
	defer ws.Close()

	// 读泵：KeepAlive 回执 + 播放进度上报。
	// 真机验证：Yamby 的进度不走 HTTP POST /Sessions/Playing*（桥接日志零记录），
	// 而是走 WebSocket 消息——不解析就会丢进度（"播放进度不准"根因）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m struct {
				MessageType string          `json:"MessageType"`
				Data        json.RawMessage `json:"Data"`
			}
			if json.Unmarshal(msg, &m) != nil {
				continue
			}
			switch m.MessageType {
			case "KeepAlive":
				_ = ws.WriteMessage(websocket.TextMessage, []byte(wsKeepAliveMsg))
			case "Sessions/Playing", "Sessions/Playing/Progress", "Sessions/Playing/Stopped":
				var d struct {
					ItemId        string `json:"ItemId"`
					PositionTicks int64  `json:"PositionTicks"`
				}
				if json.Unmarshal(m.Data, &d) == nil && d.ItemId != "" {
					log.Printf("[ws] %s item=%s ticks=%d", m.MessageType, d.ItemId, d.PositionTicks)
					h.recordToFN(r, d.ItemId, d.PositionTicks, m.MessageType == "Sessions/Playing/Stopped")
				}
			}
		}
	}()

	// 写泵：周期服务端 KeepAlive，客户端断开即退出
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := ws.WriteMessage(websocket.TextMessage, []byte(wsKeepAliveMsg)); err != nil {
				return
			}
		}
	}
}
