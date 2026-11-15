package emby_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fnos-emby-bridge/internal/emby"
	"fnos-emby-bridge/internal/fn"
	"fnos-emby-bridge/internal/mock"
)

// newPersonBridge 起 mock 飞牛 + 桥接，返回桥接 base URL。
func newPersonBridge(t *testing.T) string {
	t.Helper()
	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	t.Cleanup(fnSrv.Close)

	client := fn.NewClient(fnSrv.URL)
	if err := client.Login(t.Context(), "u", fn.SHA256Hex("p")); err != nil {
		t.Fatalf("mock 登录: %v", err)
	}
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	h.AllowAnonymous = true
	bridge := httptest.NewServer(h.Handler())
	t.Cleanup(bridge.Close)
	return bridge.URL
}

func getJSONMap(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", url, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s 不是 JSON: %v body=%s", url, err, raw)
	}
	return out
}

// itemIDs 取 QueryResult.Items[].Id。
func itemIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, _ := body["Items"].([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := m["Id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestPersonFilmography 回归「Emby 点开演员看不到其他作品」。
//
// 真机抓包（fnos-emby-bridge 请求日志 + 飞牛 nginx access.log）：
//   - 点演员后客户端请求 /Users/{uid}/Items/{personId}（人物 DTO）
//   - 再对 Movie / Series / Episode 各发一条 /Items?PersonIds=<pid>&IncludeItemTypes=...
//   - 飞牛 person/item/list 的 job 是**必填枚举**，空 job 只回 48 字节空列表；
//     桥接旧实现正好传空 job，于是作品网格永远为空。
func TestPersonFilmography(t *testing.T) {
	base := newPersonBridge(t)

	// 1. 详情页演员表带人物 guid（客户端据此跳人物页）
	detail := getJSONMap(t, base+"/Users/u/Items/fv_001?api_key=k")
	people, _ := detail["People"].([]any)
	if len(people) == 0 {
		t.Fatalf("详情页 People 为空: %v", detail)
	}
	person, _ := people[0].(map[string]any)
	if got := person["Id"]; got != "per_001" {
		t.Fatalf("People[0].Id=%v want per_001", got)
	}
	if person["PrimaryImageTag"] == nil {
		t.Errorf("People[0] 缺 PrimaryImageTag，客户端不会请求头像")
	}

	// 2. 人物 DTO：必须是 BaseItemDto（不是 QueryResult 对象），否则客户端解析失败
	b := getJSONMap(t, base+"/Persons/per_001?api_key=k")
	if b["Type"] != "Person" {
		t.Errorf("Type=%v want Person（落到兜底会变成 QueryResult 对象）", b["Type"])
	}
	if b["Name"] != "演员甲" {
		t.Errorf("Name=%v", b["Name"])
	}
	if tags, ok := b["ImageTags"].(map[string]any); !ok || tags["Primary"] == nil {
		t.Errorf("人物缺 ImageTags.Primary: %v", b["ImageTags"])
	}

	// 3. 电影作品：来自 job=Actor
	movies := getJSONMap(t, base+"/Users/u/Items?PersonIds=per_001&IncludeItemTypes=Movie"+
		"&Recursive=true&Limit=20&SortBy=SortName&SortOrder=Ascending&api_key=k")
	if ids := itemIDs(t, movies); len(ids) != 1 || ids[0] != "fv_001" {
		t.Fatalf("电影作品=%v want [fv_001]", ids)
	}

	// 4. 剧集作品：来自 job=Director——只查单个 job 会漏掉，必须按枚举合并
	series := getJSONMap(t, base+"/Users/u/Items?PersonIds=per_001&IncludeItemTypes=Series"+
		"&Recursive=true&Limit=20&api_key=k")
	if ids := itemIDs(t, series); len(ids) != 1 || ids[0] != "fv_tv" {
		t.Fatalf("剧集作品=%v want [fv_tv]", ids)
	}

	// 5. 人物头像
	resp, err := http.Get(base + "/Persons/per_001/Images/Primary?api_key=k")
	if err != nil {
		t.Fatalf("人物头像: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("人物头像 status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Fatalf("人物头像 Content-Type=%q want image/*", ct)
	}
}

// TestPersonNotFound 非人物 guid 回 404，客户端才会回落到 /Items/{id}。
func TestPersonNotFound(t *testing.T) {
	base := newPersonBridge(t)
	resp, err := http.Get(base + "/Persons/not_a_person?api_key=k")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want 404 body=%s", resp.StatusCode, body)
	}
}
