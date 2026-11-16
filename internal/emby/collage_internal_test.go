package emby

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"

	"fnos-emby-bridge/internal/fn"
)

// solidImage 纯色测试图。
func solidImage(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("拼图不是合法 JPEG: %v", err)
	}
	return img
}

func near(c color.Color, want color.RGBA, tol int) bool {
	r, g, b, _ := c.RGBA()
	return absInt(int(r>>8)-int(want.R)) <= tol &&
		absInt(int(g>>8)-int(want.G)) <= tol &&
		absInt(int(b>>8)-int(want.B)) <= tol
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestPickCollageItemsDedupesByPoster 多个条目共用同一张海报时，
// 只能选出一张——否则三栏会拼出三张一样的图。
func TestPickCollageItemsDedupesByPoster(t *testing.T) {
	raw := `[
		{"guid":"a","poster":"/p1"},
		{"guid":"b","poster":"/p1"},
		{"guid":"c","poster":"/p1"},
		{"guid":"d","poster":"/p2"},
		{"guid":"e","poster":"/p3"}
	]`
	var list []fn.MediaItem
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	picks := pickCollageItems(list, "lib_001", 1)
	if len(picks) != 3 {
		t.Fatalf("应选出 3 张不同海报，实际 %d：%v", len(picks), picks)
	}
	seen := map[string]bool{}
	for _, p := range picks {
		if seen[p.path] {
			t.Fatalf("选出了重复海报 %q", p.path)
		}
		seen[p.path] = true
	}
	for _, want := range []string{"/p1", "/p2", "/p3"} {
		if !seen[want] {
			t.Errorf("缺少海报 %q，实际 %v", want, seen)
		}
	}
}

// TestPickCollageItemsOnlyOneUnique 只有一张唯一海报时只能选出 1 张。
func TestPickCollageItemsOnlyOneUnique(t *testing.T) {
	raw := `[{"guid":"a","poster":"/p1"},{"guid":"b","poster":"/p1"}]`
	var list []fn.MediaItem
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	if picks := pickCollageItems(list, "lib_001", 1); len(picks) != 1 {
		t.Fatalf("应只选出 1 张，实际 %d", len(picks))
	}
}

// TestComposeCollageNoRepeat 不足 3 张时不再复制最后一张：
// 1 张 → 整幅铺满；3 张 → 三栏各自不同。
func TestComposeCollageNoRepeat(t *testing.T) {
	red := color.RGBA{220, 20, 20, 255}
	green := color.RGBA{20, 200, 20, 255}
	blue := color.RGBA{20, 20, 220, 255}

	// 1 张：整幅都是它（旧实现会横向复制成三份，右半仍是同一张——从颜色上看不出差别，
	// 所以这里改用「列宽」验证：把图裁成只铺中间会留黑边）。
	one, err := composeCollage([]image.Image{solidImage(300, 450, red)})
	if err != nil {
		t.Fatalf("单图拼接失败: %v", err)
	}
	img1 := decodeJPEG(t, one)
	if b := img1.Bounds(); b.Dx() != collageWidth || b.Dy() != collageHeight {
		t.Fatalf("尺寸应为 %dx%d，实际 %dx%d", collageWidth, collageHeight, b.Dx(), b.Dy())
	}
	if !near(img1.At(10, 360), red, 12) || !near(img1.At(collageWidth-10, 360), red, 12) {
		t.Fatalf("单图应整幅铺满（两端都应为海报颜色）")
	}

	// 3 张不同颜色：三栏颜色必须各不相同
	three, err := composeCollage([]image.Image{
		solidImage(300, 450, red), solidImage(300, 450, green), solidImage(300, 450, blue),
	})
	if err != nil {
		t.Fatalf("三图拼接失败: %v", err)
	}
	img3 := decodeJPEG(t, three)
	if !near(img3.At(200, 360), red, 12) {
		t.Errorf("第一栏应为红色，实际 %v", img3.At(200, 360))
	}
	if !near(img3.At(600, 360), green, 12) {
		t.Errorf("第二栏应为绿色，实际 %v", img3.At(600, 360))
	}
	if !near(img3.At(1000, 360), blue, 12) {
		t.Errorf("第三栏应为蓝色，实际 %v", img3.At(1000, 360))
	}
}

// TestCollageTagByContent 封面 tag 由「库 guid + 内容签名」决定：
// 内容不变则恒定（客户端不会重复下载），内容变化才换 tag。
func TestCollageTagByContent(t *testing.T) {
	a1 := collageTag("lib_001", "6-1700000000")
	a2 := collageTag("lib_001", "6-1700000000")
	if a1 != a2 {
		t.Fatalf("同一内容签名下 tag 应稳定：%q vs %q", a1, a2)
	}
	if !strings.HasPrefix(a1, "c") {
		t.Fatalf("封面 tag 应以 c 开头，实际 %q", a1)
	}
	if b := collageTag("lib_001", "7-1700000000"); b == a1 {
		t.Fatalf("条目数变化后 tag 应不同，实际都为 %q", b)
	}
	if c := collageTag("lib_001", "6-1700000001"); c == a1 {
		t.Fatalf("最新入库时间变化后 tag 应不同，实际都为 %q", c)
	}
}
