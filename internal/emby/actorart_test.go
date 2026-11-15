package emby_test

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fnos-emby-bridge/internal/mock"
)

// TestActorArtFallback 回归「飞牛「其他」库演员头像空白」。
//
// 飞牛不刮这类库的演员头像（person.profile 恒为空），演员图按 Kodi/Emby 约定
// 落在影片目录的 .actors/演员名.jpg。桥接要自己读出来：
//   - People[].PrimaryImageTag 带上 person 标记（否则客户端不发图片请求）
//   - /Persons/{id}/Images/Primary 回本地图
//   - /Items/{personId}/Images/Primary?tag=person:… 同样回本地图
func TestActorArtFallback(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "fv_vid.mp4")
	if err := os.WriteFile(video, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	artDir := filepath.Join(dir, ".actors")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	art := []byte("\xff\xd8\xff\xe0fake-jpeg-bytes")
	if err := os.WriteFile(filepath.Join(artDir, "演员乙.jpg"), art, 0o644); err != nil {
		t.Fatal(err)
	}
	mock.SetItemField("fv_vid", "path", video)
	t.Cleanup(func() { mock.SetItemField("fv_vid", "path", nil) })

	base := newPersonBridge(t)

	// 1. 详情页 People 必须带 tag，且是 person 标记
	detail := getJSONMap(t, base+"/Users/u/Items/fv_vid?api_key=k")
	people, _ := detail["People"].([]any)
	if len(people) == 0 {
		t.Fatalf("详情页 People 为空: %v", detail)
	}
	p0, _ := people[0].(map[string]any)
	tag, _ := p0["PrimaryImageTag"].(string)
	if tag == "" {
		t.Fatalf("People[0] 无 PrimaryImageTag，客户端不会请求头像")
	}
	decoded, err := base64.URLEncoding.DecodeString(tag)
	if err != nil || !strings.HasPrefix(string(decoded), "person:per_002") {
		t.Fatalf("PrimaryImageTag 解出 %q want person:per_002[:ver]", decoded)
	}

	// 2. /Persons 头像走本地 .actors
	body := getBody(t, base+"/Persons/per_002/Images/Primary?api_key=k")
	if string(body) != string(art) {
		t.Fatalf("/Persons 头像内容不匹配: %q", body)
	}

	// 3. /Items/{personId} 带 person tag 时也走本地图
	body2 := getBody(t, base+"/Items/per_002/Images/Primary?tag="+url.QueryEscape(tag)+"&api_key=k")
	if string(body2) != string(art) {
		t.Fatalf("/Items 头像内容不匹配: %q", body2)
	}
}

// getBody 取 200 响应的原始 body。
func getBody(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d", url, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return b
}
