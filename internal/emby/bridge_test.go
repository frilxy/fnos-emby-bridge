package emby_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

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
	bridge := httptest.NewServer(h.Routes())
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

	// 9. 验证 /Items/{id}/Progress 回传
	resp3, _ := http.Post(bridge.URL+"/Items/fv_001/Progress?api_key=k", "application/json",
		strings.NewReader(`{"PositionTicks":1200000000}`)) // 120s
	if resp3.StatusCode != 204 {
		t.Fatalf("progress status=%d", resp3.StatusCode)
	}

	t.Logf("all emby endpoints ok against mock fnos")
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
