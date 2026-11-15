package main

import (
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
