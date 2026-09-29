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
	}
	// ancestor_guid 模式（库 guid → 子条目）
	mockAncestor = map[string][]string{
		"lib_001": {"fv_001", "fv_tv"},
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
	loginCount           = map[string]int{} // username → 真登录次数
)

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
}

func NewHandler(cloudDirect bool, cdnBase string) *http.ServeMux {
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
		writeResult(w, 0, "", []map[string]any{
			{"guid": "lib_001", "title": "电影库", "category": "Movie", "view_type": 0,
				"posters": []string{"/1c/09/mock_lib.webp"}},
		})
	})

	mux.HandleFunc("POST /v/api/v1/item/list", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AncestorGuid string `json:"ancestor_guid"`
			ParentGuid   string `json:"parent_guid"`
			Page         int    `json:"page"`
			PageSize     int    `json:"page_size"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			writeResult(w, 1, "bad body", nil)
			return
		}
		var guids []string
		if req.AncestorGuid != "" {
			// 真机互斥：ancestor 模式只对库 guid 生效
			guids = mockAncestor[req.AncestorGuid]
		} else if req.ParentGuid != "" {
			guids = mockParent[req.ParentGuid]
		}
		list := make([]map[string]any, 0, len(guids))
		for _, g := range guids {
			if it, ok := mockItems[g]; ok {
				list = append(list, it)
			}
		}
		writeResult(w, 0, "", map[string]any{"list": list, "total": len(list)})
	})

	mux.HandleFunc("GET /v/api/v1/item/{guid}", func(w http.ResponseWriter, r *http.Request) {
		it, ok := mockItems[r.PathValue("guid")]
		if !ok {
			writeResult(w, 404, "no such item", nil)
			return
		}
		writeResult(w, 0, "", it)
	})

	mux.HandleFunc("GET /v/api/v1/season/list/{guid}", func(w http.ResponseWriter, r *http.Request) {
		guids := mockParent[r.PathValue("guid")]
		list := make([]map[string]any, 0, len(guids))
		for _, g := range guids {
			if it, ok := mockItems[g]; ok {
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
		it, ok := mockItems[req.ItemGuid]
		if !ok {
			it = mockItems["fv_001"]
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
		writeResult(w, 0, "", map[string]any{})
	})

	// 0.9.8 海报端点：/v/api/v1/sys/img/{path}?w=400（mock 不校验 relay cookie）
	mux.HandleFunc("GET /v/api/v1/sys/img/{rest}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png1x1())
	})

	return mux
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
