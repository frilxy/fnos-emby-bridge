package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// TestMatchActorArt 三种目录名 + 平铺/子目录两种布局 + 名字归一化。
func TestMatchActorArt(t *testing.T) {
	for _, dirName := range []string{".actors", ".actor", "actors"} {
		dir := t.TempDir()
		art := filepath.Join(dir, dirName)
		if err := os.MkdirAll(art, 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(art, "神木 麗.jpg")
		if err := os.WriteFile(want, []byte("img"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := matchActorArt(dir, actorNames("神木麗", "")); got != want {
			t.Errorf("%s 平铺: got %q want %q", dirName, got, want)
		}
	}

	// 子目录布局：.actors/<演员名>/任意图片
	dir := t.TempDir()
	sub := filepath.Join(dir, ".actors", "演员乙")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sub, "1.png")
	if err := os.WriteFile(want, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := matchActorArt(dir, actorNames("演员 乙", "")); got != want {
		t.Errorf("子目录布局: got %q want %q", got, want)
	}

	// 不该命中：别的演员、非图片
	if got := matchActorArt(dir, actorNames("别人", "")); got != "" {
		t.Errorf("不该命中: %q", got)
	}
}

// TestActorNamesDedup 译名与原名归一化后相同只保留一份。
func TestActorNamesDedup(t *testing.T) {
	if got := actorNames("神木 麗", "神木麗"); len(got) != 1 || got[0] != "神木麗" {
		t.Fatalf("actorNames=%v want [神木麗]", got)
	}
}

// TestPrepareImage 大图会被缩到 maxDim 并转成 jpeg；小图原样返回。
func TestPrepareImage(t *testing.T) {
	dir := t.TempDir()

	big := image.NewRGBA(image.Rect(0, 0, 2400, 3600))
	for y := 0; y < 3600; y += 3 {
		for x := 0; x < 2400; x += 3 {
			big.SetRGBA(x, y, color.RGBA{200, 100, 50, 255})
		}
	}
	bigPath := filepath.Join(dir, "big.png")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, big); err != nil {
		t.Fatal(err)
	}
	f.Close()

	data, name, err := prepareImage(bigPath, 1024)
	if err != nil {
		t.Fatalf("prepareImage: %v", err)
	}
	if name != "big.jpg" {
		t.Errorf("缩放后文件名应为 big.jpg，实际 %q", name)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("缩放结果不是 jpeg: %v", err)
	}
	if b := img.Bounds(); b.Dx() > 1024 || b.Dy() > 1024 {
		t.Fatalf("缩放后尺寸 %dx%d 超过 1024", b.Dx(), b.Dy())
	}

	small := image.NewRGBA(image.Rect(0, 0, 600, 900))
	smallPath := filepath.Join(dir, "small.png")
	f2, _ := os.Create(smallPath)
	_ = png.Encode(f2, small)
	f2.Close()
	data2, name2, err := prepareImage(smallPath, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if name2 != "small.png" {
		t.Errorf("小图应原样上传，文件名 %q", name2)
	}
	raw, _ := os.ReadFile(smallPath)
	if !bytes.Equal(data2, raw) {
		t.Errorf("小图不该被重编码（字节应完全一致）")
	}
}
