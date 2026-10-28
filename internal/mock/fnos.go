// Package mock 提供一个最小可用的飞牛影视 0.9.8 mock 服务，用于本地验证桥接。
// 实现契约（真机逆向 0.9.8）：
//   - POST /v/api/v2/user/loginByPassword（密码 SHA256 hex）
//   - GET  /v/api/v1/mediadb/list
//   - POST /v/api/v1/item/list（ancestor_guid / parent_guid 互斥双模式）
//   - GET  /v/api/v1/item/{guid}
//   - POST /v/api/v1/play/info
//   - POST /v/api/v1/stream（cloudDirect 分支返回夸克直链）
//   - GET  /v/api/v1/media/range/{guid}
//   - POST /v/api/v1/play/record
//   - GET  /v/api/v1/sys/img/{path}（海报，mode=relay cookie）
//
// mock 不校验 authx/token（只看桥接是否按契约调对了端点与参数）。
package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// MockCdnPath 是 mock 云盘 CDN 的相对路径，测试把它拼到独立 CDN server 地址上。
const MockCdnPath = "/mock-cdn/a.mp4"

// mock 数据树：
//
//	lib_001（库/Movie）--ancestor--> fv_001(Movie) + fv_tv(TV)
//	fv_tv（剧）--parent--> fv_s1(Season)
//	fv_s1（季）--parent--> fv_002(Episode)
var (
	mockItems = map[string]map[string]any{
		"lib_001": {"guid": "lib_001", "title": "电影库", "type": "MediaDB", "category": "Movie"},
		"fv_001": {"guid": "fv_001", "parent_guid": "lib_001", "title": "电影A", "type": "Movie",
			"poster": "/1c/09/mock_fv001.webp", "poster_width": 300, "poster_height": 450,
			"duration": 5400, "runtime": 90, "can_play": 1, "is_watched": 0,
			"season_number": 0, "episode_number": 0, "air_date": "2024-01-01", "overview": "一部电影"},
		"fv_tv": {"guid": "fv_tv", "parent_guid": "lib_001", "title": "剧集B", "type": "TV",
			"poster": "/1c/09/mock_fvtv.webp", "poster_width": 300, "poster_height": 450,
			"can_play": 0, "is_watched": 0, "number_of_seasons": 1, "overview": "一部剧"},
		"fv_s1": {"guid": "fv_s1", "parent_guid": "fv_tv", "title": "第一季", "type": "Season",
			"poster": "/1c/09/mock_fvs1.webp", "season_number": 1, "number_of_episodes": 1},
		"fv_002": {"guid": "fv_002", "parent_guid": "fv_s1", "title": "剧集B 第1集", "type": "Episode",
			"poster": "/1c/09/mock_fv002.webp", "poster_width": 300, "poster_height": 450,
			"duration": 2700, "runtime": 45, "can_play": 1, "is_watched": 0,
			"season_number": 1, "episode_number": 1, "air_date": "2024-02-01", "overview": "一集剧",
			"tv_title": "剧集B"},
		// fork 新增：Other 类库 —— 复现「点进媒体库一片空白」。
		// 飞牛 Other 类库的条目 type=Video，桥接映射为 Emby Type=Video；
		// 客户端若按 CollectionType 猜成 IncludeItemTypes=Movie,Series 就会全被过滤掉。
		"lib_other": {"guid": "lib_other", "title": "其他库", "type": "MediaDB", "category": "Other"},
		"fv_vid": {"guid": "fv_vid", "parent_guid": "lib_other", "title": "散装视频A", "type": "Video",
			"poster": "/1c/09/mock_fvvid.webp", "duration": 1800, "runtime": 30,
			"can_play": 1, "is_watched": 0, "overview": "一个散装视频"},
	}
	// ancestor_guid 模式（库 guid → 子条目）
	mockAncestor = map[string][]string{
		"lib_001":   {"fv_001", "fv_tv"},
		"lib_other": {"fv_vid"},
	}
	// parent_guid 模式（剧 → 季、季 → 集）
	mockParent = map[string][]string{
		"fv_tv": {"fv_s1"},
		"fv_s1": {"fv_002"},
	}
)

// NewHandler 返回一个可被 http.ListenAndServe 挂载的 mock 飞牛 mux。
// cloudDirect=true 时，POST /v/api/v1/stream 返回夸克云盘直链（cloud_type=4），
// 直链 URL = cdnBase + MockCdnPath（测试传独立 CDN server 地址，CDN 与飞牛分域）。
// 桥接会走 ChunkedProxy 路径直连该 URL；false 则返回本地文件，走 media/range。
// RecordEntry 一条 play/record 调用记录（多会话隔离测试断言用）。
type RecordEntry struct {
	Token string // 调用方 Authorization（飞牛侧身份）
	Guid  string
	Ts    int64
}

var (
	recordMu   sync.Mutex
	records    []RecordEntry
	loginMu    sync.Mutex
	loginCount = map[string]int{} // username → 真登录次数
	rangeMu    sync.Mutex
	lastRange  string // 最近一次 media/range 收到的 Range 头
)

// MockLastRangeHeader 返回最近一次 /v/api/v1/media/range 收到的 Range 头。
//
// fork 新增（回归测试用）：HEAD 取流必须用 bytes=0-0 向飞牛只要元数据；
// 若返回空串，说明桥接正在按「完整文件」去拉上游 —— 真机上这会把
// 整个文件当响应体推给 HEAD 请求，导致请求永不返回。
func MockLastRangeHeader() string {
	rangeMu.Lock()
	defer rangeMu.Unlock()
	return lastRange
}

// MockRecords 返回并清空 play/record 记录。
func MockRecords() []RecordEntry {
	recordMu.Lock()
	defer recordMu.Unlock()
	out := records
	records = nil
	return out
}

// MockLoginCount 返回某用户名的真登录次数（不重置）。
func MockLoginCount(username string) int {
	loginMu.Lock()
	defer loginMu.Unlock()
	return loginCount[username]
}

// MockReset 清空全部计数/记录（测试隔离用）。
func MockReset() {
	recordMu.Lock()
	records = nil
	recordMu.Unlock()
	loginMu.Lock()
	loginCount = map[string]int{}
	loginMu.Unlock()
	rangeMu.Lock()
	lastRange = ""
	rangeMu.Unlock()
}

// Option 调整 mock 提供的夹具（默认与历史行为完全一致，避免影响既有断言）。
type Option func(*mockOpts)

type mockOpts struct {
	otherLibrary bool
	tvLibrary    bool
	brokenDetail string
	brokenSearch bool
}

// WithOtherLibrary 额外提供一个 Other 类库（条目 type=Video）。
// 用于回归「点进媒体库一片空白」：Other 类库此前不声明 CollectionType，
// 客户端猜成 IncludeItemTypes=Movie,Series 会把 Type=Video 条目全过滤掉。
func WithOtherLibrary() Option {
	return func(o *mockOpts) { o.otherLibrary = true }
}

// WithBrokenDetail 让 item/{guid} 对指定条目返回 404（验证 play/info 详情兜底）。
func WithBrokenDetail(guid string) Option {
	return func(o *mockOpts) { o.brokenDetail = guid }
}

// WithTVLibrary 增加一个 TV 类库（含季/集），用于验证 Latest 剧集语义与 NextUp。
func WithTVLibrary() Option {
	return func(o *mockOpts) { o.tvLibrary = true }
}

// WithBrokenSearch 让 /search/list 返回 500，验证桥接的库内标题搜索兜底。
func WithBrokenSearch() Option {
	return func(o *mockOpts) { o.brokenSearch = true }
}

// addTVLibrary 往**当前 handler 的**夹具里补 TV 库/剧/季/集。
func addTVLibrary(items map[string]map[string]any, ancestor, parent map[string][]string) {
	items["lib_tv"] = map[string]any{"guid": "lib_tv", "title": "剧集库", "type": "MediaDB", "category": "TV"}
	items["fv_tv2"] = map[string]any{"guid": "fv_tv2", "parent_guid": "lib_tv", "title": "剧集C", "type": "TV",
		"poster": "/1c/09/mock_fvtv2.webp", "poster_width": 300, "poster_height": 450,
		"can_play": 0, "is_watched": 0, "number_of_seasons": 1, "number_of_episodes": 2,
		"overview": "一部剧", "genres": []int{1}, "content_ratings": "TV-14",
		"production_countries": []string{"CN"}, "original_title": "Drama C", "trim_id": "tm12345",
		"create_time": 1700000000, "air_date": "2020-05-01"}
	items["fv_s2"] = map[string]any{"guid": "fv_s2", "parent_guid": "fv_tv2", "title": "第一季", "type": "Season",
		"poster": "/1c/09/mock_fvs2.webp", "season_number": 1, "number_of_episodes": 2}
	items["fv_ep1"] = map[string]any{"guid": "fv_ep1", "parent_guid": "fv_s2", "ancestor_guid": "lib_tv",
		"title": "剧集C 第1集", "type": "Episode", "poster": "/1c/09/mock_ep1.webp",
		"duration": 2400, "runtime": 40, "can_play": 1, "is_watched": 0, "ts": 600,
		"season_number": 1, "episode_number": 1, "air_date": "2020-05-01", "overview": "第一集",
		"tv_title": "剧集C", "create_time": 1700000100, "genres": []int{1},
		"media_stream": map[string]any{"resolutions": []string{"1080p"}, "audio_type": "Stereo"}}
	items["fv_ep2"] = map[string]any{"guid": "fv_ep2", "parent_guid": "fv_s2", "ancestor_guid": "lib_tv",
		"title": "剧集C 第2集", "type": "Episode", "poster": "/1c/09/mock_ep2.webp",
		"duration": 2400, "runtime": 40, "can_play": 1, "is_watched": 0, "ts": 0,
		"season_number": 1, "episode_number": 2, "air_date": "2020-05-08", "overview": "第二集",
		"tv_title": "剧集C", "create_time": 1700000200, "genres": []int{1},
		"media_stream": map[string]any{"resolutions": []string{"4k"}, "audio_type": "DolbyAtmos"}}
	ancestor["lib_tv"] = []string{"fv_tv2"}
	parent["fv_tv2"] = []string{"fv_s2"}
	parent["fv_s2"] = []string{"fv_ep1", "fv_ep2"}
}

func NewHandler(cloudDirect bool, cdnBase string, opts ...Option) *http.ServeMux {
	var o mockOpts
	for _, fn := range opts {
		fn(&o)
	}
	// 每个 handler 用独立 map（浅拷贝：条目值仍是共享指针，SetItemField 能生效），
	// 避免 WithTVLibrary 的夹具污染其它测试。
	items := make(map[string]map[string]any, len(mockItems))
	for k, v := range mockItems {
		items[k] = v
	}
	ancestor := make(map[string][]string, len(mockAncestor))
	for k, v := range mockAncestor {
		ancestor[k] = append([]string(nil), v...)
	}
	parent := make(map[string][]string, len(mockParent))
	for k, v := range mockParent {
		parent[k] = append([]string(nil), v...)
	}
	if o.tvLibrary {
		addTVLibrary(items, ancestor, parent)
	}
	registerItemMap(items)
	mux := http.NewServeMux()

	// 0.9.8 登录：v2 + SHA256 密码
	// 多会话测试：token 按用户名生成（MOCKTOKEN_<user>_<登录次数>），
	// 并全局计数——断言「客户端刷新不重复真登录」用。
	mux.HandleFunc("POST /v/api/v2/user/loginByPassword", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			AppName  string `json:"app_name"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if req.Password == "" || len(req.Password) != 64 {
			writeResult(w, 1, "bad password hash", nil)
			return
		}
		loginMu.Lock()
		loginCount[req.Username]++
		n := loginCount[req.Username]
		loginMu.Unlock()
		writeResult(w, 0, "", map[string]any{"token": fmt.Sprintf("MOCKTOKEN_%s_%d", req.Username, n)})
	})

	mux.HandleFunc("GET /v/api/v1/mediadb/list", func(w http.ResponseWriter, r *http.Request) {
		libs := []map[string]any{
			{"guid": "lib_001", "title": "电影库", "category": "Movie", "view_type": 0,
				"posters": []string{"/1c/09/mock_lib.webp"}},
		}
		if o.otherLibrary {
			libs = append(libs, map[string]any{
				"guid": "lib_other", "title": "其他库", "category": "Other", "view_type": 0,
				"posters": []string{"/1c/09/mock_lib.webp"}})
		}
		if o.tvLibrary {
			libs = append(libs, map[string]any{
				"guid": "lib_tv", "title": "剧集库", "category": "TV", "view_type": 0,
				"posters": []string{"/1c/09/mock_lib.webp"}})
		}
		writeResult(w, 0, "", libs)
	})

	mux.HandleFunc("POST /v/api/v1/item/list", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AncestorGuid string `json:"ancestor_guid"`
			ParentGuid   string `json:"parent_guid"`
			Page         int    `json:"page"`
			PageSize     int    `json:"page_size"`
			Tags         struct {
				Type    []string `json:"type"`
				Genres  int      `json:"genres"`
				Watched string   `json:"watched"`
			} `json:"tags"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			writeResult(w, 1, "bad body", nil)
			return
		}
		var guids []string
		if req.AncestorGuid != "" {
			// 真机互斥：ancestor 模式只对库 guid 生效
			guids = ancestor[req.AncestorGuid]
			// 集是库的子孙，mockAncestor 只存直接容器；按 Episode 查询时从全局夹具补。
			if len(req.Tags.Type) == 1 && req.Tags.Type[0] == "Episode" {
				guids = nil
				for g, it := range items {
					if str(it["ancestor_guid"]) == req.AncestorGuid && str(it["type"]) == "Episode" {
						guids = append(guids, g)
					}
				}
			}
		} else if req.ParentGuid != "" {
			guids = parent[req.ParentGuid]
		}
		list := make([]map[string]any, 0, len(guids))
		for _, g := range guids {
			it, ok := items[g]
			if !ok {
				continue
			}
			if len(req.Tags.Type) > 0 && !containsStr(req.Tags.Type, str(it["type"])) {
				continue
			}
			if req.Tags.Genres > 0 && !containsInt(it["genres"], req.Tags.Genres) {
				continue
			}
			if req.Tags.Watched != "" {
				want := req.Tags.Watched == "1"
				got := it["is_watched"] == 1 || it["watched"] == 1
				if want != got {
					continue
				}
			}
			list = append(list, it)
		}
		sort.SliceStable(list, func(i, j int) bool {
			return int64Of(list[i]["create_time"]) > int64Of(list[j]["create_time"])
		})
		writeResult(w, 0, "", map[string]any{"list": list, "total": len(list)})
	})

	mux.HandleFunc("GET /v/api/v1/item/{guid}", func(w http.ResponseWriter, r *http.Request) {
		it, ok := items[r.PathValue("guid")]
		if !ok {
			writeResult(w, 404, "no such item", nil)
			return
		}
		writeResult(w, 0, "", it)
	})

	mux.HandleFunc("GET /v/api/v1/season/list/{guid}", func(w http.ResponseWriter, r *http.Request) {
		guids := parent[r.PathValue("guid")]
		list := make([]map[string]any, 0, len(guids))
		for _, g := range guids {
			if it, ok := items[g]; ok {
				list = append(list, it)
			}
		}
		writeResult(w, 0, "", list)
	})

	mux.HandleFunc("POST /v/api/v1/play/info", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ItemGuid string `json:"item_guid"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		it, ok := items[req.ItemGuid]
		if !ok {
			it = items["fv_001"]
		}
		writeResult(w, 0, "", map[string]any{
			"guid": it["guid"], "parent_guid": it["parent_guid"], "grand_guid": "",
			"type": it["type"], "media_guid": "md_" + str(it["guid"]),
			"video_guid": "v_001", "audio_guid": "a_001", "subtitle_guid": "no_display",
			"ts":   0,
			"item": it,
		})
	})

	// POST /v/api/v1/stream（云盘直链接口，fntv-electron api.ts:407）
	// cloudDirect=true：返回夸克直链（cloud_type=4）+ mock CDN 地址，桥接走 ChunkedProxy。
	// cloudDirect=false：返回本地文件（cloud_storage_info=null），桥接走 media/range。
	mux.HandleFunc("POST /v/api/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		if cloudDirect {
			writeResult(w, 0, "", map[string]any{
				"file_stream": map[string]any{"guid": "md_001", "path": "cloud", "size": 4096, "can_play": 1},
				"qualities":   []map[string]any{{"bitrate": 8000, "resolution": "1080p", "progressive": true, "is_m3u8": false}},
				"cloud_storage_info": map[string]any{
					"cloud_storage_type": 4, // 夸克
					"valid":              true, "is_vip": true,
				},
				"header": map[string]any{
					"Cookie":     []string{"quark_session=abc"},
					"User-Agent": []string{"quark_mock_ua"},
				},
				"direct_link_qualities": []map[string]any{{
					"url":        cdnBase + MockCdnPath,
					"expired_at": 0,
					"resolution": "1080p",
					"is_m3u8":    false,
				}},
				"direct_link_audio_streams": []any{},
			})
			return
		}
		writeResult(w, 0, "", map[string]any{
			"file_stream":               map[string]any{"guid": "md_001", "path": "/vol1/media/a.mp4", "size": 4096, "can_play": 1},
			"video_stream":              map[string]any{},
			"audio_streams":             []any{},
			"subtitle_streams":          []any{},
			"qualities":                 []map[string]any{{"bitrate": 8000, "resolution": "1080p", "progressive": true, "is_m3u8": false}},
			"cloud_storage_info":        nil,
			"header":                    map[string]any{"Cookie": []string{}},
			"direct_link_qualities":     []any{},
			"direct_link_audio_streams": []any{},
		})
	})

	// 模拟视频字节流（真实飞牛是 mp4 文件，这里回 4KB 假数据；正确处理 Range）
	mux.HandleFunc("GET /v/api/v1/media/range/{guid}", func(w http.ResponseWriter, r *http.Request) {
		rangeMu.Lock()
		lastRange = r.Header.Get("Range")
		rangeMu.Unlock()
		total := int64(4096)
		buf := make([]byte, total)
		for i := range buf {
			buf[i] = byte(i % 256)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		if rh := r.Header.Get("Range"); rh != "" {
			spec := strings.TrimPrefix(rh, "bytes=")
			if comma := strings.Index(spec, ","); comma >= 0 {
				spec = spec[:comma]
			}
			parts := strings.SplitN(spec, "-", 2)
			start, end := int64(0), total-1
			if len(parts) == 2 {
				if parts[0] == "" && parts[1] != "" {
					// 后缀形式 bytes=-N：取末尾 N 字节
					var n int64
					fmt.Sscanf(parts[1], "%d", &n)
					if n > total {
						n = total
					}
					start, end = total-n, total-1
				} else if parts[0] != "" {
					fmt.Sscanf(parts[0], "%d", &start)
					if parts[1] != "" {
						fmt.Sscanf(parts[1], "%d", &end)
					}
				}
			}
			if start > total-1 {
				start = total - 1
			}
			if end > total-1 {
				end = total - 1
			}
			if start < 0 {
				start = 0
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(buf[start : end+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(total))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf)
	})

	// item/watched：标记/取消已看（桥接"播完自动标已看"会调用；POST 标记、DELETE 取消）
	mux.HandleFunc("POST /v/api/v1/item/watched", func(w http.ResponseWriter, r *http.Request) {
		mockSetWatched(r, 1)
		writeResult(w, 0, "", map[string]any{})
	})
	mux.HandleFunc("DELETE /v/api/v1/item/watched", func(w http.ResponseWriter, r *http.Request) {
		mockSetWatched(r, 0)
		writeResult(w, 0, "", map[string]any{})
	})

	mux.HandleFunc("POST /v/api/v1/play/record", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		var req struct {
			ItemGuid string  `json:"item_guid"`
			Ts       float64 `json:"ts"`
		}
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &req)
		}
		recordMu.Lock()
		records = append(records, RecordEntry{Token: token, Guid: req.ItemGuid, Ts: int64(req.Ts)})
		recordMu.Unlock()
		itemMu.Lock()
		setItemFieldLocked(req.ItemGuid, "ts", int64(req.Ts))
		itemMu.Unlock()
		writeResult(w, 0, "", map[string]any{})
	})

	// 搜索：GET /search/list?q=
	mux.HandleFunc("GET /v/api/v1/search/list", func(w http.ResponseWriter, r *http.Request) {
		if o.brokenSearch {
			http.Error(w, "search unavailable", http.StatusInternalServerError)
			return
		}
		q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		list := make([]map[string]any, 0)
		for _, it := range items {
			if q == "" || strings.Contains(strings.ToLower(str(it["title"])), q) ||
				strings.Contains(strings.ToLower(str(it["tv_title"])), q) {
				if str(it["type"]) == "MediaDB" {
					continue
				}
				list = append(list, it)
			}
		}
		writeResult(w, 0, "", list)
	})

	// 播放历史：GET /play/list（有 ts 的条目，最近播放在前）
	mux.HandleFunc("GET /v/api/v1/play/list", func(w http.ResponseWriter, r *http.Request) {
		list := make([]map[string]any, 0)
		for _, it := range items {
			if int64Of(it["ts"]) > 0 {
				list = append(list, it)
			}
		}
		sort.SliceStable(list, func(i, j int) bool {
			return int64Of(list[i]["ts"]) > int64Of(list[j]["ts"])
		})
		writeResult(w, 0, "", list)
	})

	// 筛选字典
	mux.HandleFunc("GET /v/api/v1/tag/genres", func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, 0, "", []map[string]any{
			{"id": 1, "value": "剧情"}, {"id": 2, "value": "科幻"},
		})
	})
	mux.HandleFunc("GET /v/api/v1/tag/list", func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, 0, "", map[string]any{
			"genres": []int{1, 2}, "resolutions": []string{"4k", "1080p"},
			"color_range": []string{"SDR", "HDR"}, "audio_type": []string{"Stereo", "DolbyAtmos"},
			"locate": []string{"CN"}, "decades": []string{"2020s", "Recent"},
		})
	})

	// 流列表：供详情/字幕使用（字幕 index=3 为外挂 srt）
	mux.HandleFunc("GET /v/api/v1/stream/list/{guid}", func(w http.ResponseWriter, r *http.Request) {
		guid := r.PathValue("guid")
		writeResult(w, 0, "", map[string]any{
			"files": []map[string]any{{
				"guid": "md_" + guid, "file_name": "a.mkv", "size": 4096, "can_play": 1,
				"create_time": 1700000000000,
			}},
			"video_streams": []map[string]any{{
				"media_guid": "md_" + guid, "codec_name": "hevc", "width": 1920, "height": 1080,
				"bps": 8000000, "profile": "Main 10", "bit_depth": 10, "duration": 2400,
			}},
			"audio_streams": []map[string]any{{
				"media_guid": "md_" + guid, "index": 1, "codec_name": "aac", "language": "chi",
				"channels": 2, "is_default": 1, "title": "国语",
			}},
			"subtitle_streams": []map[string]any{
				{"media_guid": "md_" + guid, "guid": "sub_001", "index": 3, "codec_name": "subrip",
					"format": "srt", "language": "chi", "title": "中文", "is_external": 1, "is_default": 1},
				{"media_guid": "md_" + guid, "guid": "sub_002", "index": 4, "codec_name": "ass",
					"format": "ass", "language": "chi", "title": "内嵌", "is_external": 0},
			},
		})
	})
	mux.HandleFunc("GET /v/api/v1/subtitle/dl/{guid}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
		_, _ = w.Write([]byte("1\n00:00:01,000 --> 00:00:03,000\nmock subtitle\n"))
	})

	// 0.9.8 海报端点：/v/api/v1/sys/img/{path}?w=400（mock 不校验 relay cookie）
	mux.HandleFunc("GET /v/api/v1/sys/img/{rest}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png1x1())
	})

	return mux
}

// itemMu 保护 mock 条目写操作；itemMaps 记录每个 handler 的本地条目表，
// 让 SetItemField / item/watched / play/record 能同时作用到所有活跃 handler。
var (
	itemMu   sync.Mutex
	itemMaps []map[string]map[string]any
)

func registerItemMap(m map[string]map[string]any) {
	itemMu.Lock()
	defer itemMu.Unlock()
	itemMaps = append(itemMaps, m)
}

// setItemFieldLocked 修改全局夹具与所有活跃 handler 的本地副本。
func setItemFieldLocked(guid, key string, val any) {
	if it, ok := mockItems[guid]; ok {
		it[key] = val
	}
	for _, m := range itemMaps {
		if it, ok := m[guid]; ok {
			it[key] = val
		}
	}
}

// mockSetWatched 修改 mock 条目的 is_watched/watched（并发安全）。
func mockSetWatched(r *http.Request, v int) {
	var req struct {
		ItemGuid string `json:"item_guid"`
	}
	if b, err := io.ReadAll(r.Body); err == nil {
		_ = json.Unmarshal(b, &req)
	}
	if req.ItemGuid == "" {
		return
	}
	itemMu.Lock()
	defer itemMu.Unlock()
	setItemFieldLocked(req.ItemGuid, "is_watched", v)
	setItemFieldLocked(req.ItemGuid, "watched", v)
}

// SetItemField 供测试直接改 mock 条目字段（例如把 ts 设成片尾，复现
// "位置到片尾却仍未标已看"的「继续观看」残留）。
func SetItemField(guid, key string, val any) {
	itemMu.Lock()
	defer itemMu.Unlock()
	setItemFieldLocked(guid, key, val)
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func containsInt(v any, want int) bool {
	raw, ok := v.([]int)
	if !ok {
		return false
	}
	for _, n := range raw {
		if n == want {
			return true
		}
	}
	return false
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// writeResult 输出飞牛统一响应包 {"code":0,"msg":"","data":...}。
func writeResult(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg, "data": data})
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// 1x1 透明 PNG，用于模拟 poster
func png1x1() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
		0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2B, 0x3E, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}
}
