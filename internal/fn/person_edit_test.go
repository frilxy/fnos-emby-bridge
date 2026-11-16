package fn_test

import (
	"testing"

	"fnos-emby-bridge/internal/fn"
)

// TestPersonEditAndUpload 演员图灌注脚本依赖的三个接口：
// image/temp/upload → getEditDetail → saveEditDetail。
func TestPersonEditAndUpload(t *testing.T) {
	srv := newMockFN(t)
	c := fn.NewClient(srv.URL)
	if err := c.Login(t.Context(), "u", fn.SHA256Hex("p")); err != nil {
		t.Fatalf("登录: %v", err)
	}

	// 1. 上传图片拿 hash_path
	hash, err := c.UploadImage(t.Context(), "演员乙.jpg", "poster", []byte("fake-jpeg-bytes"))
	if err != nil {
		t.Fatalf("上传: %v", err)
	}
	if hash == "" {
		t.Fatal("hash_path 为空")
	}

	// 2. 读人物编辑详情（要保留原字段）
	detail, err := c.PersonEditDetail(t.Context(), "per_002")
	if err != nil {
		t.Fatalf("getEditDetail: %v", err)
	}
	if detail["name"] != "演员乙" {
		t.Fatalf("getEditDetail 应带原字段，实际 %v", detail)
	}
	if got := detail["profile_path"]; got != "" {
		t.Fatalf("per_002 初始 profile_path 应为空，实际 %v", got)
	}

	// 3. 只改 profile_path 回写，再读回确认
	detail["profile_path"] = hash
	if err := c.SavePersonEditDetail(t.Context(), detail); err != nil {
		t.Fatalf("saveEditDetail: %v", err)
	}
	after, err := c.PersonEditDetail(t.Context(), "per_002")
	if err != nil {
		t.Fatalf("getEditDetail(2): %v", err)
	}
	if after["profile_path"] != hash {
		t.Fatalf("profile_path 未写入，实际 %v", after["profile_path"])
	}
	if after["name"] != "演员乙" {
		t.Fatalf("回写不应丢失其它字段，实际 %v", after)
	}
}

// newMockFN 起一个 mock 飞牛服务。
func newMockFN(t *testing.T) *mockServer {
	t.Helper()
	srv := mockServer{}
	srv.start(t)
	return &srv
}
