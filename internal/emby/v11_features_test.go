package emby_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fnos-emby-bridge/internal/emby"
	"fnos-emby-bridge/internal/fn"
	"fnos-emby-bridge/internal/mock"
)

// newV11Bridge 起一个 strict-auth 桥接（默认 AllowAnonymous=false），并登录一个用户。
func newV11Bridge(t *testing.T, opts ...mock.Option) (*httptest.Server, string) {
	t.Helper()
	t.Chdir(t.TempDir()) // 会话/用户状态文件落临时目录
	mock.MockReset()

	fnSrv := httptest.NewServer(mock.NewHandler(false, "", opts...))
	t.Cleanup(fnSrv.Close)

	client := fn.NewClient(fnSrv.URL)
	if err := client.Login(t.Context(), "u", fn.SHA256Hex("p")); err != nil {
		t.Fatalf("fn login: %v", err)
	}
	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler())
	t.Cleanup(bridge.Close)
	return bridge, loginToken(t, bridge.URL, "u", "p")
}

func loginToken(t *testing.T, base, user, pass string) string {
	t.Helper()
	resp, err := http.Post(base+"/Users/AuthenticateByName", "application/json",
		strings.NewReader(fmt.Sprintf(`{"UserName":%q,"Pwd":%q}`, user, pass)))
	if err != nil {
		t.Fatalf("login %s: %v", user, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("login %s status=%d body=%s", user, resp.StatusCode, b)
	}
	var out struct {
		AccessToken string `json:"AccessToken"`
	}
	if json.Unmarshal(b, &out) != nil || out.AccessToken == "" {
		t.Fatalf("login %s decode: %s", user, b)
	}
	return out.AccessToken
}

func getWithToken(t *testing.T, url, token string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Emby-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s status=%d body=%s", url, resp.StatusCode, b)
	}
	return b
}

func postWithToken(t *testing.T, method, url, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("X-Emby-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func decodeItems(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var q struct {
		Items []map[string]any `json:"Items"`
	}
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatalf("decode items: %v (%s)", err, b)
	}
	return q.Items
}

func idsOf(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprint(it["Id"]))
	}
	return out
}

func TestSearchTermReturnsMatches(t *testing.T) {
	bridge, token := newV11Bridge(t)
	body := getWithToken(t, bridge.URL+"/Users/u/Items?SearchTerm=%E7%94%B5%E5%BD%B1&IncludeItemTypes=Movie", token)
	items := decodeItems(t, body)
	ids := idsOf(items)
	if len(ids) == 0 || ids[0] != "fv_001" {
		t.Fatalf("搜索『电影』应命 fv_001，实际 %v（body=%s）", ids, body)
	}
	for _, it := range items {
		if fmt.Sprint(it["Id"]) == "fv_tv" {
			t.Fatalf("搜索 Movie 时不应返回 Series：%v", ids)
		}
	}
}

func TestDateCreatedUsesCreateTimeNotPremiere(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	// fv_001：入库晚、首播早；fv_tv2：入库早、首播晚。
	mock.SetItemField("fv_001", "create_time", 1800000000)
	mock.SetItemField("fv_001", "air_date", "2000-01-01")
	mock.SetItemField("fv_tv2", "create_time", 1600000000)
	mock.SetItemField("fv_tv2", "air_date", "2024-01-01")

	body := getWithToken(t, bridge.URL+"/Items?IncludeItemTypes=Movie,Series&SortBy=DateCreated&SortOrder=Descending", token)
	ids := idsOf(decodeItems(t, body))
	if len(ids) < 2 || ids[0] != "fv_001" {
		t.Fatalf("按 DateCreated 降序应先 fv_001（入库晚），实际 %v", ids)
	}

	body = getWithToken(t, bridge.URL+"/Items?IncludeItemTypes=Movie,Series&SortBy=PremiereDate&SortOrder=Descending", token)
	ids = idsOf(decodeItems(t, body))
	if len(ids) < 2 || ids[0] != "fv_tv2" {
		t.Fatalf("按 PremiereDate 降序应先 fv_tv2（首播晚），实际 %v", ids)
	}

	// 详情里的 DateCreated 应来自 create_time
	body = getWithToken(t, bridge.URL+"/Items/fv_001", token)
	var it map[string]any
	_ = json.Unmarshal(body, &it)
	dc, _ := it["DateCreated"].(string)
	if !strings.HasPrefix(dc, "2027") { // 1800000000 ≈ 2027-01
		t.Fatalf("DateCreated 未使用 create_time：%q", dc)
	}

	// 元数据：原始标题 / 分级 / 国家 / TMDB ProviderId
	body = getWithToken(t, bridge.URL+"/Items/fv_tv2", token)
	_ = json.Unmarshal(body, &it)
	if fmt.Sprint(it["OriginalTitle"]) != "Drama C" || fmt.Sprint(it["OfficialRating"]) != "TV-14" {
		t.Fatalf("原始标题/分级未映射：OriginalTitle=%v OfficialRating=%v", it["OriginalTitle"], it["OfficialRating"])
	}
	props, _ := it["ProductionLocations"].([]any)
	if len(props) == 0 || fmt.Sprint(props[0]) != "CN" {
		t.Fatalf("国家未映射：%v", it["ProductionLocations"])
	}
	pid, _ := it["ProviderIds"].(map[string]any)
	if fmt.Sprint(pid["Tmdb"]) != "12345" {
		t.Fatalf("TMDB ProviderId 未映射：%v", pid)
	}
}

func TestMultiKeySortAndRandomPaging(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	// ProductionYear 是主键：fv_tv2=2020，fv_001=2000
	mock.SetItemField("fv_001", "air_date", "2000-01-01")
	body := getWithToken(t, bridge.URL+"/Items?IncludeItemTypes=Movie,Series&SortBy=ProductionYear,PremiereDate&SortOrder=Descending", token)
	ids := idsOf(decodeItems(t, body))
	if len(ids) == 0 || ids[0] != "fv_tv2" {
		t.Fatalf("多键排序应先按 ProductionYear 倒序（fv_tv2=2020）：%v", ids)
	}

	// Random 分页：两页合起来应覆盖全部且不重复
	page1 := getWithToken(t, bridge.URL+"/Items?IncludeItemTypes=Movie,Series&SortBy=Random&Limit=2&StartIndex=0", token)
	page2 := getWithToken(t, bridge.URL+"/Items?IncludeItemTypes=Movie,Series&SortBy=Random&Limit=2&StartIndex=2", token)
	all := append(idsOf(decodeItems(t, page1)), idsOf(decodeItems(t, page2))...)
	seen := map[string]bool{}
	for _, id := range all {
		if seen[id] {
			t.Fatalf("Random 分页出现重复条目：%v（p1=%v p2=%v）", all, idsOf(decodeItems(t, page1)), idsOf(decodeItems(t, page2)))
		}
		seen[id] = true
	}
	if len(all) < 3 {
		t.Fatalf("Random 分页条目不足：%v", all)
	}
}

func TestIs4KAndYearFilters(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	body := getWithToken(t, bridge.URL+"/Items?Is4K=true&Recursive=true&IncludeItemTypes=Episode", token)
	ids := idsOf(decodeItems(t, body))
	if len(ids) != 1 || ids[0] != "fv_ep2" {
		t.Fatalf("Is4K 应只返回 4K 的 fv_ep2：%v（%s）", ids, body)
	}
	body = getWithToken(t, bridge.URL+"/Items?Years=2020&IncludeItemTypes=Movie,Series", token)
	ids = idsOf(decodeItems(t, body))
	found := false
	for _, id := range ids {
		if id == "fv_tv2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Years=2020 应包含 fv_tv2：%v", ids)
	}
}

func TestGenresFiltersAndDictionary(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	mock.SetItemField("fv_001", "genres", []int{2})

	body := getWithToken(t, bridge.URL+"/Genres", token)
	var g struct {
		Items []map[string]any `json:"Items"`
	}
	if err := json.Unmarshal(body, &g); err != nil || len(g.Items) == 0 {
		t.Fatalf("/Genres 应返回类型字典：%s", body)
	}

	body = getWithToken(t, bridge.URL+"/Items?Genres=%E5%89%A7%E6%83%85&IncludeItemTypes=Movie,Series", token)
	ids := idsOf(decodeItems(t, body))
	found := false
	for _, id := range ids {
		if id == "fv_tv2" {
			found = true
		}
		if id == "fv_001" {
			t.Fatalf("genres=剧情 不应包含 genres=科幻 的 fv_001：%v", ids)
		}
	}
	if !found {
		t.Fatalf("genres=剧情 应包含 fv_tv2：%v", ids)
	}

	body = getWithToken(t, bridge.URL+"/Items/Filters?ParentId=lib_tv", token)
	var f map[string]any
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("/Items/Filters decode: %s", body)
	}
	genres, _ := f["Genres"].([]any)
	years, _ := f["Years"].([]any)
	ratings, _ := f["OfficialRatings"].([]any)
	if len(genres) == 0 || len(years) == 0 || len(ratings) == 0 {
		t.Fatalf("/Items/Filters 应包含 Genres/Years/OfficialRatings：%s", body)
	}
}

func TestLatestTVReturnsEpisodes(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	body := getWithToken(t, bridge.URL+"/Users/u/Items/Latest?ParentId=lib_tv", token)
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("Latest 应返回纯数组：%v (%s)", err, body)
	}
	if len(items) != 2 {
		t.Fatalf("TV 库 Latest 应返回 2 集，实际 %v（%s）", idsOf(items), body)
	}
	for _, it := range items {
		if fmt.Sprint(it["Type"]) != "Episode" {
			t.Fatalf("TV 库 Latest 应返回 Episode，实际 %v", it["Type"])
		}
		if fmt.Sprint(it["SeriesId"]) != "fv_tv2" {
			t.Fatalf("Latest 集缺 SeriesId：%v", it["SeriesId"])
		}
	}
	if fmt.Sprint(items[0]["Id"]) != "fv_ep2" { // create_time 更大
		t.Fatalf("Latest 应按入库时间倒序，首条应是 fv_ep2：%v", idsOf(items))
	}
}

func TestNextUpGlobal(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	body := getWithToken(t, bridge.URL+"/Shows/NextUp?UserId=u", token)
	items := decodeItems(t, body)
	if len(items) == 0 {
		t.Fatalf("全局 NextUp 不应为空")
	}
	if fmt.Sprint(items[0]["SeriesId"]) != "fv_tv2" {
		t.Fatalf("NextUp 应带 SeriesId：%s", body)
	}
}

func TestResumeFromPlayList(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	body := getWithToken(t, bridge.URL+"/Users/u/Items/Resume", token)
	ids := idsOf(decodeItems(t, body))
	found := false
	for _, id := range ids {
		if id == "fv_ep1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Resume 应使用 /play/list 返回有进度的 fv_ep1：%v（%s）", ids, body)
	}
}

func TestExternalSubtitleDelivery(t *testing.T) {
	bridge, token := newV11Bridge(t)
	body := getWithToken(t, bridge.URL+"/Videos/fv_001/x/Subtitles/3/Stream.srt", token)
	if !strings.Contains(string(body), "mock subtitle") {
		t.Fatalf("外挂字幕内容不对：%s", body)
	}

	code, _ := postWithToken(t, http.MethodGet, bridge.URL+"/Videos/fv_001/x/Subtitles/4/Stream.ass", token)
	if code != http.StatusNotFound {
		t.Fatalf("内嵌字幕应 404（由客户端直读容器），实际 %d", code)
	}
}

func TestImageLongPathAndBackdropTags(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithTVLibrary())
	mock.SetItemField("fv_tv2", "backdrops", []string{"/a.webp", "/b.webp"})

	body := getWithToken(t, bridge.URL+"/Items/fv_tv2", token)
	var it map[string]any
	_ = json.Unmarshal(body, &it)
	tags, _ := it["BackdropImageTags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("多背景图应给 2 个 BackdropImageTags：%v", it["BackdropImageTags"])
	}

	req, _ := http.NewRequest(http.MethodGet, bridge.URL+"/Items/fv_tv2/Images/Backdrop/1/tag/jpg/400/600", nil)
	req.Header.Set("X-Emby-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("长路径图片应 200，实际 %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("图片应带 ETag")
	}
	req2, _ := http.NewRequest(http.MethodGet, bridge.URL+"/Items/fv_tv2/Images/Backdrop/1/tag/jpg/400/600", nil)
	req2.Header.Set("X-Emby-Token", token)
	req2.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("image 304: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match 应 304，实际 %d", resp2.StatusCode)
	}
}

func TestRatingAndHideFromResume(t *testing.T) {
	bridge, token := newV11Bridge(t)
	code, body := postWithToken(t, http.MethodPost, bridge.URL+"/Users/u/Items/fv_001/Rating?Likes=true", token)
	if code != 200 || !strings.Contains(string(body), `"Likes":true`) {
		t.Fatalf("Rating 应返回 Likes=true：%d %s", code, body)
	}

	body = getWithToken(t, bridge.URL+"/Users/u/Items/fv_001", token)
	var it map[string]any
	_ = json.Unmarshal(body, &it)
	ud, _ := it["UserData"].(map[string]any)
	if like, _ := ud["Likes"].(bool); !like {
		t.Fatalf("详情 UserData.Likes 应为 true：%v", ud)
	}
	body = getWithToken(t, bridge.URL+"/Items?Filters=Likes&IncludeItemTypes=Movie", token)
	if !strings.Contains(string(body), "fv_001") {
		t.Fatalf("Filters=Likes 应返回已点赞的 fv_001：%s", body)
	}

	// 让 fv_001 有进度，然后隐藏；Resume 不应再返回它
	mock.SetItemField("fv_001", "ts", 120)
	body = getWithToken(t, bridge.URL+"/Users/u/Items/Resume", token)
	if !strings.Contains(string(body), "fv_001") {
		t.Fatalf("隐藏前 Resume 应包含 fv_001：%s", body)
	}
	code, _ = postWithToken(t, http.MethodPost, bridge.URL+"/Users/u/Items/fv_001/HideFromResume?Hide=true", token)
	if code != 200 {
		t.Fatalf("HideFromResume 应 200，实际 %d", code)
	}
	body = getWithToken(t, bridge.URL+"/Users/u/Items/Resume", token)
	if strings.Contains(string(body), "fv_001") {
		t.Fatalf("隐藏后 Resume 不应包含 fv_001：%s", body)
	}
}

func TestAuthenticateByIDAndDetailFallback(t *testing.T) {
	bridge, token := newV11Bridge(t, mock.WithBrokenDetail("fv_001"))

	// play/info 兜底：item/{guid} 404 时详情仍应 200
	body := getWithToken(t, bridge.URL+"/Items/fv_001", token)
	if !strings.Contains(string(body), `"Id":"fv_001"`) || strings.Contains(string(body), `"Error"`) {
		t.Fatalf("item 详情 404 时应回退 play/info：%s", body)
	}

	// /Users/{id}/Authenticate 必须免 token（客户端刷新入口）
	me := getWithToken(t, bridge.URL+"/Users/Me", token)
	var user map[string]any
	_ = json.Unmarshal(me, &user)
	uid := fmt.Sprint(user["Id"])
	if uid == "" {
		t.Fatalf("/Users/Me 缺 Id：%s", me)
	}
	req, _ := http.NewRequest(http.MethodPost, bridge.URL+"/Users/"+uid+"/Authenticate",
		strings.NewReader(`{"Pw":"p"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "AccessToken") {
		t.Fatalf("/Users/{id}/Authenticate 应免 token 且返回 token：%d %s", resp.StatusCode, b)
	}
}

func TestAuthRequiredAndUserIdentity(t *testing.T) {
	bridge, token := newV11Bridge(t)

	// 公开端点仍可匿名访问
	if b := getJSON(t, bridge.URL+"/System/Info/Public"); !strings.Contains(string(b), "ServerName") {
		t.Fatalf("公开端点应可匿名：%s", b)
	}
	// 无 token 的媒体端点必须 401
	resp, err := http.Get(bridge.URL + "/Items")
	if err != nil {
		t.Fatalf("no-token items: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token /Items 应 401，实际 %d", resp.StatusCode)
	}
	// 错误 token 也必须 401
	resp, err = http.Get(bridge.URL + "/Items?api_key=wrong")
	if err != nil {
		t.Fatalf("bad-token items: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 token /Items 应 401，实际 %d", resp.StatusCode)
	}
	// 登录后可用
	if b := getWithToken(t, bridge.URL+"/Items", token); len(b) == 0 {
		t.Fatalf("有效 token /Items 应返回内容")
	}
	// Emby 的 X-Emby-Authorization 头（部分客户端不单独发 X-Emby-Token）
	req, _ := http.NewRequest(http.MethodGet, bridge.URL+"/Items", nil)
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Emby", Device="test", Token="`+token+`"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("X-Emby-Authorization: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("X-Emby-Authorization 应通过鉴权，实际 %d", resp.StatusCode)
	}

	// 多用户身份
	alice := loginToken(t, bridge.URL, "alice", "pa")
	bob := loginToken(t, bridge.URL, "bob", "pb")
	body := getWithToken(t, bridge.URL+"/Users/Me", alice)
	var me map[string]any
	_ = json.Unmarshal(body, &me)
	if fmt.Sprint(me["Name"]) != "alice" || fmt.Sprint(me["Id"]) == "" {
		t.Fatalf("/Users/Me 应返回 alice 的身份：%s", body)
	}
	body = getWithToken(t, bridge.URL+"/Users/Me", bob)
	_ = json.Unmarshal(body, &me)
	if fmt.Sprint(me["Name"]) != "bob" {
		t.Fatalf("/Users/Me 应返回 bob 的身份：%s", body)
	}
	public := getJSON(t, bridge.URL+"/Users/Public")
	if !strings.Contains(string(public), "alice") || !strings.Contains(string(public), "bob") {
		t.Fatalf("/Users/Public 应包含已登录用户：%s", public)
	}
}
