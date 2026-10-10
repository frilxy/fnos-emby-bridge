package emby_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fnos-emby-bridge/internal/emby"
	"fnos-emby-bridge/internal/fn"
	"fnos-emby-bridge/internal/mock"
)

// 起 mock 飞牛 + 桥接，逐个验证 Emby 端点。
func TestBridgeAgainstMockFN(t *testing.T) {
	// 1. mock 飞牛服务（本地文件模式，走 media/range）
	fnMux := mock.NewHandler(false, "")
	fnSrv := httptest.NewServer(fnMux)
	defer fnSrv.Close()

	// 2. 桥接 handler（不真启动，直接调 handler）
	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))

	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	// 用 Handler() 而非 Routes()：生产入口是 Handler()（含 /emby 剥离、大小写归一、
	// 畸形绝对 URL 还原三层中间件），裸路由表测不到这些中间件的行为。
	bridge := httptest.NewServer(h.Handler())
	defer bridge.Close()

	// 3. 验证 /System/Info
	assertStatus(t, bridge.URL+"/System/Info?api_key=k", 200)

	// 4. 验证 /Users/AuthenticateByName（会真去 mock 飞牛登录）
	resp, err := http.Post(bridge.URL+"/Users/AuthenticateByName", "application/json",
		strings.NewReader(`{"UserName":"u","Pwd":"p"}`))
	if err != nil {
		t.Fatalf("auth post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("auth status=%d body=%s", resp.StatusCode, body)
	}
	var auth struct {
		Token string `json:"Token"`
	}
	_ = json.Unmarshal(body, &auth)
	if auth.Token == "" {
		t.Fatalf("auth no token: %s", body)
	}

	// 5. 验证 /Items（聚合 seed）
	body = getJSON(t, bridge.URL+"/Items?api_key=k")
	var items struct {
		Items []map[string]any `json:"Items"`
	}
	_ = json.Unmarshal(body, &items)
	if len(items.Items) == 0 {
		t.Fatalf("items empty: %s", body)
	}
	// 校验每个 item 都有 Id/Name/Type
	for i, it := range items.Items {
		if it["Id"] == nil || it["Name"] == nil {
			t.Fatalf("item %d missing Id/Name: %v", i, it)
		}
	}

	// 6. 验证 /Items/{id}
	body = getJSON(t, bridge.URL+"/Items/fv_001?api_key=k")
	var one struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	_ = json.Unmarshal(body, &one)
	if one.ID != "fv_001" {
		t.Fatalf("item id=%q", one.ID)
	}

	// 7. 验证 /Videos/{id}/Playback
	body = getJSON(t, bridge.URL+"/Videos/fv_001/Playback?api_key=k")
	var pb struct {
		StreamURL string `json:"StreamUrl"`
	}
	_ = json.Unmarshal(body, &pb)
	if !strings.Contains(pb.StreamURL, "/Videos/fv_001/stream") {
		t.Fatalf("stream url=%q", pb.StreamURL)
	}

	// 8. 验证 /Videos/{id}/Stream 透传字节流
	req, _ := http.NewRequest("GET", bridge.URL+"/Videos/fv_001/Stream?api_key=k", nil)
	req.Header.Set("Range", "bytes=0-100")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream get: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 && resp2.StatusCode != 206 {
		t.Fatalf("stream status=%d", resp2.StatusCode)
	}
	streamBytes, _ := io.ReadAll(resp2.Body)
	if len(streamBytes) == 0 {
		t.Fatalf("stream empty")
	}

	// 8b. 回归（fork 修复）：HEAD 取流必须立即返回元数据，不能挂起。
	// 上游实现把 GET 路由一并匹配 HEAD，HEAD 无 Range 时把整个文件当响应体
	// 往外推、而 HEAD 不发送响应体，请求永不返回（真机表现为 curl 超时、
	// 部分播放器探测失败后判定「无法播放」）。
	headClient := &http.Client{Timeout: 5 * time.Second}
	hreq, _ := http.NewRequest("HEAD", bridge.URL+"/Videos/fv_001/stream?api_key=k", nil)
	hresp, err := headClient.Do(hreq)
	if err != nil {
		t.Fatalf("HEAD stream 未能及时返回（疑似挂起）: %v", err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != 200 {
		t.Fatalf("HEAD stream status=%d", hresp.StatusCode)
	}
	if got := hresp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("HEAD stream accept-ranges=%q", got)
	}
	if cl := hresp.Header.Get("Content-Length"); cl == "" || cl == "0" {
		t.Fatalf("HEAD stream content-length=%q", cl)
	}
	// 关键判据：HEAD 只能向飞牛要 1 字节，绝不能按「完整文件」拉上游。
	// mock 只有 4KB，光看响应快慢区分不出上游实现差异，所以直接断言上游收到的 Range。
	if got := mock.MockLastRangeHeader(); got != "bytes=0-0" {
		t.Fatalf("HEAD 向上游请求的 Range=%q，应为 bytes=0-0（否则真机上 HEAD 会拉整个文件并挂死）", got)
	}

	// 8c. HEAD + Range 应回 206 且 Content-Range 合法
	hreq2, _ := http.NewRequest("HEAD", bridge.URL+"/Videos/fv_001/stream?api_key=k", nil)
	hreq2.Header.Set("Range", "bytes=0-99")
	hresp2, err := headClient.Do(hreq2)
	if err != nil {
		t.Fatalf("HEAD(range) stream: %v", err)
	}
	hresp2.Body.Close()
	if hresp2.StatusCode != 206 {
		t.Fatalf("HEAD(range) status=%d", hresp2.StatusCode)
	}
	if cr := hresp2.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-99/") {
		t.Fatalf("HEAD(range) content-range=%q", cr)
	}
	if got := mock.MockLastRangeHeader(); got != "bytes=0-0" {
		t.Fatalf("HEAD(range) 向上游请求的 Range=%q，应为 bytes=0-0", got)
	}

	// 8d. 畸形绝对 URL 路径的回归测试见 TestMangledAbsoluteURLPathRepaired（独立函数）

	// 9. 验证 /Items/{id}/Progress 回传
	resp3, _ := http.Post(bridge.URL+"/Items/fv_001/Progress?api_key=k", "application/json",
		strings.NewReader(`{"PositionTicks":1200000000}`)) // 120s
	if resp3.StatusCode != 204 {
		t.Fatalf("progress status=%d", resp3.StatusCode)
	}

	t.Logf("all emby endpoints ok against mock fnos")
}

// 回归（fork 修复）：客户端把「绝对播放地址」拼到自己 base 后面，会产生
// /embyhttp:/host/Videos/{id}/stream 这类畸形路径（Go 把请求行里的 // 归一成 /）。
//
// 上游行为：该路径不匹配任何路由 → 落到兜底 handler → 对 GET 返回
// 200 + 空 QueryResult JSON。播放器拿到 49 字节 JSON 而非视频字节，
// 而且状态码是 200，客户端连报错都没有，表现为「点开视频无法播放」。
//
// 修复：入站把 scheme+host 摘掉还原成真实路径，让请求进入 handleStream。
// 独立成函数是为了能在上游代码上单独跑，确认它确实抓得到这个问题。
func TestMangledAbsoluteURLPathRepaired(t *testing.T) {
	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler())
	defer bridge.Close()

	mangled := &http.Request{
		Method: "GET",
		URL: &url.URL{
			Scheme:   "http",
			Host:     strings.TrimPrefix(bridge.URL, "http://"),
			Path:     "/embyhttp:/192.168.10.229:8096/Videos/fv_001/stream",
			RawQuery: "MediaSourceId=fv_001&Static=true",
		},
		Header: http.Header{},
	}
	mangled.Header.Set("Range", "bytes=0-100")

	resp, err := http.DefaultClient.Do(mangled)
	if err != nil {
		t.Fatalf("畸形路径请求: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		t.Fatalf("畸形路径 status=%d body=%s", resp.StatusCode, body)
	}
	// 最关键的判据：绝不能是 JSON 兜底响应
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "json") {
		t.Fatalf("畸形路径落到了兜底(JSON)，未进入 handleStream: status=%d ct=%q body=%s",
			resp.StatusCode, ct, body)
	}
	if len(body) == 0 {
		t.Fatalf("畸形路径未返回视频字节: status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// 还原后仍应把客户端的 Range 正常透传给上游
	if got := mock.MockLastRangeHeader(); got != "bytes=0-100" {
		t.Fatalf("畸形路径还原后上游 Range=%q，应为 bytes=0-100", got)
	}
}

// 回归（fork 修复）：飞牛 Other 类库（界面里叫「其他视频」）在 Emby 里没有对应类型，
// 按 Emby 官方定义归为**混合内容**——即不声明 CollectionType
// （"If CollectionType is null, it indicates a mixed movie/tv folder"）。
//
// 混合库会让客户端自行猜 IncludeItemTypes（多为 Movie,Series），而该类库条目是
// Type=Video → 猜错就返回 0 条，表现为「点进媒体库一片空白，但首页能看到这些视频」。
// 该问题由 handleItems 的库级浏览空结果兜底解决，不靠伪造 CollectionType。
func TestOtherLibraryNotBlank(t *testing.T) {
	fnSrv := httptest.NewServer(mock.NewHandler(false, "", mock.WithOtherLibrary()))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler())
	defer bridge.Close()

	var views struct {
		Items []map[string]any `json:"Items"`
	}
	var items struct {
		Items            []map[string]any `json:"Items"`
		TotalRecordCount int              `json:"TotalRecordCount"`
	}

	// 1) Other 类库应作为混合内容：不声明 CollectionType
	body := getJSON(t, bridge.URL+"/Users/fnos-user/Views?api_key=k")
	_ = json.Unmarshal(body, &views)
	found := false
	for _, v := range views.Items {
		if v["Id"] != "lib_other" {
			continue
		}
		found = true
		if ct, ok := v["CollectionType"]; ok && ct != nil && ct != "" {
			t.Fatalf("Other 类库应按混合内容处理（不声明 CollectionType），实际=%v；"+
				"上报非空值（尤其 \"mixed\"）不在 Emby 的 CollectionType 响应枚举内，部分客户端会加载不了该库", ct)
		}
	}
	if !found {
		t.Fatalf("Views 里没有 lib_other：%s", body)
	}

	// 2) 客户端按猜错的类型（Movie,Series）请求时，库级浏览不能返回空白
	body = getJSON(t, bridge.URL+"/Users/fnos-user/Items?ParentId=lib_other&IncludeItemTypes=Movie,Series&api_key=k")
	_ = json.Unmarshal(body, &items)
	if len(items.Items) == 0 {
		t.Fatalf("IncludeItemTypes=Movie,Series 时 Other 库返回空白（应回退为该库真实内容）：%s", body)
	}

	// 3) 类型正确时（Video）走正常路径
	body = getJSON(t, bridge.URL+"/Users/fnos-user/Items?ParentId=lib_other&IncludeItemTypes=Video&api_key=k")
	_ = json.Unmarshal(body, &items)
	if len(items.Items) == 0 {
		t.Fatalf("IncludeItemTypes=Video 时 Other 库应有条目：%s", body)
	}

	// 4) 兜底只作用于库级浏览：不带 ParentId 的全局类型筛选仍应严格
	body = getJSON(t, bridge.URL+"/Users/fnos-user/Items?IncludeItemTypes=MusicAlbum&api_key=k")
	_ = json.Unmarshal(body, &items)
	if len(items.Items) != 0 {
		t.Fatalf("不带 ParentId 时不应触发空结果兜底：%s", body)
	}
}

// 回归（fork 修复）：客户端「打开媒体库 / 影视详情」必经、而此前缺失或形状不符的端点。
//
// 参照物是飞牛原生 Jellyfin 面（8005）—— 飞牛自己实现了这些端点给第三方客户端用，
// 说明真实客户端（含小幻影视）确实会调用；桥接此前要么落到空兜底、要么被通配路由
// 吃掉返回 404，导致媒体库/详情打不开。
func TestClientCompatEndpoints(t *testing.T) {
	fnSrv := httptest.NewServer(mock.NewHandler(false, "", mock.WithOtherLibrary()))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler())
	defer bridge.Close()

	// 1) /System/Info/Public：Id 必须是 32 位 hex（Emby/Jellyfin 的 ServerId 形态），
	//    且补齐 LocalAddress / OperatingSystem / StartupWizardCompleted
	body := getJSON(t, bridge.URL+"/System/Info/Public")
	var pub map[string]any
	_ = json.Unmarshal(body, &pub)
	id, _ := pub["Id"].(string)
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("/System/Info/Public Id=%q，应为 32 位 hex（非法 ServerId 会让部分客户端登记失败）", id)
	}
	for _, k := range []string{"LocalAddress", "OperatingSystem", "StartupWizardCompleted", "ServerName", "Version", "ProductName"} {
		if _, ok := pub[k]; !ok {
			t.Fatalf("/System/Info/Public 缺字段 %s：%s", k, body)
		}
	}

	// 2) /UserViews 必须返回真实媒体库（此前落到兜底 → 空列表 → 客户端看不到库）
	body = getJSON(t, bridge.URL+"/UserViews?api_key=k")
	var qr struct {
		Items []map[string]any `json:"Items"`
	}
	_ = json.Unmarshal(body, &qr)
	if len(qr.Items) == 0 {
		t.Fatalf("/UserViews 返回空媒体库列表（客户端看不到任何库）：%s", body)
	}

	// 3) /Items/Filters 与 /Users/{uid}/Items/Filters 必须存在且是 QueryFilters 对象
	//    （此前被 /Items/{id} 通配吃掉，拿 "Filters" 当影片 ID 查飞牛 → 404）
	for _, p := range []string{"/Items/Filters", "/Users/u/Items/Filters", "/items/filters"} {
		body = getJSON(t, bridge.URL+p+"?api_key=k")
		var f map[string]any
		if err := json.Unmarshal(body, &f); err != nil {
			t.Fatalf("%s 不是 JSON 对象：%s", p, body)
		}
		if _, bad := f["Error"]; bad {
			t.Fatalf("%s 返回错误（客户端打开媒体库会失败）：%s", p, body)
		}
		for _, k := range []string{"Genres", "Tags", "OfficialRatings", "Years"} {
			if _, ok := f[k]; !ok {
				t.Fatalf("%s 缺字段 %s（应为 Emby QueryFilters）：%s", p, k, body)
			}
		}
	}

	// 4) 这几条 Emby 返回纯数组，给 QueryResult 对象会让客户端解析崩
	for _, p := range []string{"/Plugins", "/Library/VirtualFolders", "/Users/u/GroupingOptions"} {
		body = getJSON(t, bridge.URL+p+"?api_key=k")
		if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			t.Fatalf("%s 应返回纯数组：%s", p, body)
		}
	}

	// 5) /Items/Counts 是 Emby 的 ItemCounts 对象，不是 QueryResult
	body = getJSON(t, bridge.URL+"/Items/Counts?api_key=k")
	var counts map[string]any
	_ = json.Unmarshal(body, &counts)
	if _, ok := counts["ItemCount"]; !ok {
		t.Fatalf("/Items/Counts 缺 ItemCount（应返回 Emby ItemCounts 对象）：%s", body)
	}
	if _, ok := counts["Items"]; ok {
		t.Fatalf("/Items/Counts 不应返回 QueryResult：%s", body)
	}

	// 6) Emby 里返回**数组**的端点：给 QueryResult 对象会让客户端按数组反序列化失败。
	//    实测小幻影视请求 /Items/{id}/Images 拿到对象后，详情页一直加载。
	for _, p := range []string{
		"/Items/fv_001/Images",
		"/Items/fv_001/LocalTrailers",
		"/Items/fv_001/Ancestors",
		"/Localization/Options",
		"/Localization/Countries",
		"/Localization/ParentalRatings",
	} {
		body = getJSON(t, bridge.URL+p+"?api_key=k")
		if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			t.Fatalf("%s 应返回纯数组（Emby 用数组，给对象客户端会解析失败）：%s", p, body)
		}
	}

	// 7) AdditionalParts / CriticReviews 官方是 QueryResult **对象**（不是数组）。
	//    实机证据：小幻影视把 AdditionalParts 反序列化成 EmbyQueryResult<EmbyMediaItem>，
	//    给数组报 "The JSON value could not be converted to ...EmbyQueryResult`1[...]"。
	for _, p := range []string{
		"/Videos/fv_001/AdditionalParts",
		"/Items/fv_001/CriticReviews",
	} {
		body = getJSON(t, bridge.URL+p+"?api_key=k")
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			t.Fatalf("%s 应返回 QueryResult 对象（Emby 官方契约），不是数组：%s", p, body)
		}
		var qr map[string]any
		if err := json.Unmarshal(body, &qr); err != nil {
			t.Fatalf("%s 响应不是 JSON 对象：%s", p, body)
		}
		if _, ok := qr["Items"]; !ok {
			t.Fatalf("%s 缺 Items 字段，不是 QueryResult：%s", p, body)
		}
	}

	// 8) /Items/{id}/Images 必须是真实的 ImageInfo 列表，不能只是空数组
	body = getJSON(t, bridge.URL+"/Items/fv_001/Images")
	var imgs []map[string]any
	_ = json.Unmarshal(body, &imgs)
	if len(imgs) == 0 || imgs[0]["ImageType"] == nil {
		t.Fatalf("/Items/{id}/Images 应返回含 ImageType 的 ImageInfo[]：%s", body)
	}
}

// 回归（fork 修复）：/Movies/Recommendations 官方返回 RecommendationDto[]（数组）。
//
// 官方契约（dev.emby.media → MoviesService/getMoviesRecommendations）：
//
//	200 | RecommendationDto[] | Returning a RecommendationDto[] object.
//
// 此前该路径未注册 → 落到兜底返回 QueryResult 对象 {"Items":[],...}，
// 客户端（小幻影视）按数组反序列化直接失败，界面表现「生成推荐影片失败」。
func TestMovieRecommendationsShape(t *testing.T) {
	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler())
	defer bridge.Close()

	// 大小写两种写法都要命中（客户端全程小写）
	for _, p := range []string{"/Movies/Recommendations", "/movies/recommendations"} {
		body := getJSON(t, bridge.URL+p+"?api_key=k")
		if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			t.Fatalf("%s 应返回 RecommendationDto[] 数组（Emby 官方契约；给对象客户端会解析失败）：%s", p, body)
		}
		var recs []map[string]any
		if err := json.Unmarshal(body, &recs); err != nil {
			t.Fatalf("%s 响应不是 JSON 数组：%s", p, body)
		}
		for i, rec := range recs {
			for _, k := range []string{"Items", "RecommendationType", "BaselineItemName", "CategoryId"} {
				if _, ok := rec[k]; !ok {
					t.Fatalf("%s 第 %d 条 RecommendationDto 缺字段 %s：%s", p, i, k, body)
				}
			}
		}
	}
}

// 回归（fork 修复）：写操作的响应必须是 200 + UserItemDataDto 本体。
//
// 官方契约（betadev.emby.media → PlaystateService）：
//
//	POST/DELETE /Users/{UserId}/PlayedItems/{Id} → 200 + UserItemDataDto
//	"Operation successful. Returning a UserItemDataDto object."
//
// 此前桥接返回 204 空响应，「标记未看」在 .NET 客户端（小幻影视）被判为操作失败；
// FavoriteItems 则返回了自造的 {"Id":..,"UserData":{..}} 包装对象，同样不是官方模型。
func TestWriteEndpointsReturnUserItemData(t *testing.T) {
	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler())
	defer bridge.Close()

	cases := []struct{ method, path, body string }{
		{"DELETE", "/Users/u/PlayedItems/fv_001", ""},
		{"POST", "/Users/u/PlayedItems/fv_001", ""},
		{"POST", "/Users/u/FavoriteItems/fv_001", ""},
		{"DELETE", "/Users/u/FavoriteItems/fv_001", ""},
		{"POST", "/Users/u/Items/fv_001/UserData", `{"Played":false}`},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, bridge.URL+c.path, strings.NewReader(c.body))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s 请求失败: %v", c.method, c.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s 应返回 200（官方契约是 200 + UserItemDataDto；204/空响应会被 .NET 客户端判为操作失败），实际 %d",
				c.method, c.path, resp.StatusCode)
		}
		var dto map[string]any
		if err := json.Unmarshal(body, &dto); err != nil {
			t.Fatalf("%s %s 响应不是 JSON 对象: %s", c.method, c.path, body)
		}
		if _, wrapped := dto["UserData"]; wrapped {
			t.Fatalf("%s %s 返回了包装对象，应为 UserItemDataDto 本体: %s", c.method, c.path, body)
		}
		// ServerId 官方注释 "Used only by our Windows app" —— Windows 客户端会读
		for _, k := range []string{"PlaybackPositionTicks", "PlayCount", "IsFavorite", "Played", "Key", "ItemId", "ServerId"} {
			if _, ok := dto[k]; !ok {
				t.Fatalf("%s %s 的 UserItemDataDto 缺字段 %s: %s", c.method, c.path, k, body)
			}
		}
	}
}

// 回归（fork 修复）：UserData 的必需字段必须齐全。
//
// 客户端（实测小幻影视 Sprout 的 ProductMetadataUserStateProjection.
// EnsureRequiredUserState）会校验 PlaybackPositionTicks / PlayCount / IsFavorite /
// Played，任一为 null 就抛
// InvalidOperationException: Emby item '...' returned incomplete required user state，
// 详情页直接加载失败。此前只在「已看完」时才写 PlayCount，未看过的条目缺该字段 ——
// 这是「Yamby 正常但小幻影视打不开详情」的根因。
func TestUserDataRequiredFields(t *testing.T) {
	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler())
	defer bridge.Close()

	// 覆盖电影 / 剧 / 集 与两条 item 路由
	for _, p := range []string{
		"/Users/u/Items/fv_001?EnableUserData=true",
		"/Users/u/Items/fv_tv?EnableUserData=true",
		"/Users/u/Items/fv_002?EnableUserData=true",
		"/Items/fv_001?EnableUserData=true",
		// 列表响应（走 Items 数组分支）
		"/Users/u/Items?Recursive=true&IncludeItemTypes=Movie&EnableUserData=true",
	} {
		body := getJSON(t, bridge.URL+p)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		checkUD := func(where string, ud map[string]any) {
			for _, k := range []string{"PlaybackPositionTicks", "PlayCount", "IsFavorite", "Played", "Key"} {
				if _, ok := ud[k]; !ok {
					t.Fatalf("%s 的 UserData 缺必需字段 %s（客户端会抛 incomplete required user state）：%s", where, k, body)
				}
			}
		}

		if ud, ok := payload["UserData"].(map[string]any); ok {
			checkUD(p, ud)
			continue
		}
		// 列表响应：逐个条目校验
		items, _ := payload["Items"].([]any)
		if len(items) == 0 {
			t.Fatalf("%s 无条目可校验：%s", p, body)
		}
		for i, raw := range items {
			it, _ := raw.(map[string]any)
			ud, ok := it["UserData"].(map[string]any)
			if !ok {
				t.Fatalf("%s 第 %d 条缺 UserData：%s", p, i, body)
			}
			checkUD(p, ud)
		}
	}
}

// 云盘直链路径：mock 返回夸克直链，桥接应直连 mock CDN（ChunkedProxy），透传字节。
func TestBridgeCloudDirectLink(t *testing.T) {
	// CDN 与飞牛同域：先建 mock 飞牛服务，再用其 URL 作为 cdnBase。
	// 这里用"两段式"：先起空 mux 占端口，再替换成完整 handler。
	// 简化：直接用 httptest 自指——先建一个临时 server 拿 URL，再建正式 handler。
	// 但 NewHandler 需要 cdnBase（即 mock 自身 URL），形成鸡生蛋。
	// 解法：让 cdnBase 指向 mock 自身，用 httptest.NewServer 时 handler 读 request.Host 拼 URL 不可行。
	// 改用：CDN 端点路径 MockCdnPath 挂在本 mock 上，直链 URL 用相对路径 + 测试前缀。
	// 最简可行：用 "http://127.0.0.1:PORT" 占位，先起 server 拿端口。
	// 这里直接两次 NewServer：第一次拿 URL，第二次建带该 URL 的 handler 再指向同端口不可行。
	// 务实做法：cdnBase 用 mockCdnPath 的相对形式，桥接直连时补全 scheme/host 由测试注入。
	// => 改为：handler 里直链 URL 直接写死指向第二次起的 CDN server。

	// 步骤 A：起一个 CDN-only server（只服务 /mock-cdn/a.mp4），记录命中次数。
	var cdnHits int64
	cdnMux := http.NewServeMux()
	cdnMux.HandleFunc("GET /mock-cdn/a.mp4", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cdnHits, 1)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		buf := make([]byte, 4096)
		for i := range buf {
			buf[i] = byte(i % 256)
		}
		if rh := r.Header.Get("Range"); rh != "" {
			start, end := parseTestRange(rh, int64(len(buf)))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(buf)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(buf[start : end+1])
		} else {
			w.Header().Set("Content-Length", fmt.Sprint(len(buf)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buf)
		}
	})
	cdnSrv := httptest.NewServer(cdnMux)
	defer cdnSrv.Close()
	cdnURL := cdnSrv.URL

	// 步骤 B：mock 飞牛（云盘直链模式），direct_link 指向 CDN server
	fnMux := mock.NewHandler(true, cdnURL)
	fnSrv := httptest.NewServer(fnMux)
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))

	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Routes())
	defer bridge.Close()

	// 不带 Range：触发 chunked 探测（bytes=0-0 取 size）+ 全量搬运
	req, _ := http.NewRequest("GET", bridge.URL+"/Videos/fv_001/Stream?api_key=k", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("cloud stream status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) == 0 {
		t.Fatalf("cloud stream empty")
	}

	// 带 Range：只取一段，验证 chunked 裁剪
	req2, _ := http.NewRequest("GET", bridge.URL+"/Videos/fv_001/Stream?api_key=k", nil)
	req2.Header.Set("Range", "bytes=0-99")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("range stream: %v", err)
	}
	defer resp2.Body.Close()
	got2, _ := io.ReadAll(resp2.Body)
	if len(got2) == 0 {
		t.Fatalf("range cloud stream empty")
	}
	if atomic.LoadInt64(&cdnHits) == 0 {
		t.Fatalf("CDN was never hit — cloud direct link path not exercised")
	}
	t.Logf("cloud direct link ok: full=%d bytes, range=%d bytes, cdnHits=%d",
		len(got), len(got2), atomic.LoadInt64(&cdnHits))
}

// 上游无视 Range 恒回 200 全量时的裁剪回归测试（夸克 chunked 路径）。
// 桥接必须丢弃 start 前字节、只回请求区间，Content-Range/206 正确。
func TestChunkedClampWhenUpstreamIgnoresRange(t *testing.T) {
	// CDN 对探测（bytes=0-0）回 206 报总大小，对其余请求无视 Range 恒回 200 全量
	// —— 命中 chunked 路径的"上游 200 裁剪"分支
	cdnMux := http.NewServeMux()
	cdnMux.HandleFunc("GET /mock-cdn/a.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/4096")
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
			return
		}
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 4096)
		for i := range buf {
			buf[i] = byte(i % 256)
		}
		_, _ = w.Write(buf)
	})
	cdnSrv := httptest.NewServer(cdnMux)
	defer cdnSrv.Close()

	fnSrv := httptest.NewServer(mock.NewHandler(true, cdnSrv.URL))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "u", fn.SHA256Hex("p"))
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Routes())
	defer bridge.Close()

	req, _ := http.NewRequest("GET", bridge.URL+"/Videos/fv_001/Stream?api_key=k", nil)
	req.Header.Set("Range", "bytes=100-199")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("want 206, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 100 {
		t.Fatalf("want 100 bytes, got %d", len(got))
	}
	for i, b := range got {
		if int(b) != (100+i)%256 {
			t.Fatalf("byte %d mismatch: got %d want %d（裁剪偏移错误）", i, b, (100+i)%256)
		}
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 100-199/") {
		t.Fatalf("Content-Range=%q", cr)
	}
}

// 0.9.8 新契约回归：Views 映射 mediadb/list、guid 下钻（库 ancestor / 剧季 parent 回退）、
// 海报走 sys/img 转发。
func TestBridgeNewContract(t *testing.T) {
	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	defer fnSrv.Close()

	client := fn.NewClient(fnSrv.URL)
	if err := client.Login(t.Context(), "u", fn.SHA256Hex("p")); err != nil {
		t.Fatalf("login: %v", err)
	}
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Routes())
	defer bridge.Close()

	// 1. Views：mediadb/list → CollectionFolder 列表
	body := getJSON(t, bridge.URL+"/Users/u/Views?api_key=k")
	var views struct {
		Items []map[string]any `json:"Items"`
	}
	_ = json.Unmarshal(body, &views)
	if len(views.Items) != 1 || views.Items[0]["Id"] != "lib_001" {
		t.Fatalf("views wrong: %s", body)
	}

	// 2. 下钻库 guid（ancestor 模式命中，无需回退）
	body = getJSON(t, bridge.URL+"/Items?ParentId=lib_001&api_key=k")
	var libItems struct {
		Items []map[string]any `json:"Items"`
	}
	_ = json.Unmarshal(body, &libItems)
	if len(libItems.Items) != 2 {
		t.Fatalf("lib drill-down want 2 items: %s", body)
	}

	// 3. 下钻剧 guid：ancestor 为空 → 自动回退 parent → Season
	body = getJSON(t, bridge.URL+"/Items?ParentId=fv_tv&api_key=k")
	var seasons struct {
		Items []map[string]any `json:"Items"`
	}
	_ = json.Unmarshal(body, &seasons)
	if len(seasons.Items) != 1 || seasons.Items[0]["Type"] != "Season" {
		t.Fatalf("tv drill-down want 1 season: %s", body)
	}

	// 4. 下钻季 guid：回退 parent → Episode
	body = getJSON(t, bridge.URL+"/Items?ParentId=fv_s1&api_key=k")
	var eps struct {
		Items []map[string]any `json:"Items"`
	}
	_ = json.Unmarshal(body, &eps)
	if len(eps.Items) != 1 || eps.Items[0]["Type"] != "Episode" {
		t.Fatalf("season drill-down want 1 episode: %s", body)
	}

	// 5. 海报：item poster 走 sys/img 转发（mock 回 webp 1x1 PNG 字节）
	resp, err := http.Get(bridge.URL + "/Items/fv_001/Images/Primary?api_key=k")
	if err != nil {
		t.Fatalf("poster get: %v", err)
	}
	defer resp.Body.Close()
	pb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(pb) == 0 {
		t.Fatalf("poster status=%d len=%d", resp.StatusCode, len(pb))
	}

	// 6. 详情：ItemDetail（GET item/{guid}）而不是 play/info
	body = getJSON(t, bridge.URL+"/Items/fv_tv?api_key=k")
	var tv struct {
		ID   string `json:"Id"`
		Type string `json:"Type"`
	}
	_ = json.Unmarshal(body, &tv)
	if tv.ID != "fv_tv" || tv.Type != "Series" {
		t.Fatalf("item detail wrong: %s", body)
	}

	t.Logf("0.9.8 contract regression ok")
}

// parseTestRange 简化 Range 解析，供测试用。
func parseTestRange(rangeHdr string, total int64) (int64, int64) {
	start, end := int64(0), total-1
	if rangeHdr == "" {
		return start, end
	}
	spec := strings.TrimPrefix(rangeHdr, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) == 2 && parts[0] != "" {
		if a, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
			start = a
		}
		if parts[1] != "" {
			if b, err := strconv.ParseInt(parts[1], 10, 64); err == nil && b < total {
				end = b
			}
		}
	}
	if start > total-1 {
		start = total - 1
	}
	if end > total-1 {
		end = total - 1
	}
	return start, end
}

// --- helpers ---

func getJSON(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func assertStatus(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s status=%d want=%d body=%s", url, resp.StatusCode, want, body)
	}
}
