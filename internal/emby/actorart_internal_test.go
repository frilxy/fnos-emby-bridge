package emby

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNormActorName 演员名归一化：空格/全角空格/下划线/点/连字符都不影响比对。
func TestNormActorName(t *testing.T) {
	cases := [][2]string{
		{"神木麗", "神木麗"},
		{"神木 麗", "神木麗"},
		{"神木_麗", "神木麗"},
		{"S1 女優", "s1女優"},
		{"姬咲 華", "姬咲華"},
	}
	for _, c := range cases {
		if got := normActorName(c[0]); got != c[1] {
			t.Errorf("normActorName(%q)=%q want %q", c[0], got, c[1])
		}
	}
}

// TestMatchActorArt 目录约定：.actors / .actor / actors 都认，
// 文件名按归一化匹配（空格、下划线差异不影响）。
func TestMatchActorArt(t *testing.T) {
	for _, sub := range []string{".actors", ".actor", "actors"} {
		dir := t.TempDir()
		art := filepath.Join(dir, sub)
		if err := os.MkdirAll(art, 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(art, "神木 麗.jpg")
		if err := os.WriteFile(want, []byte("img"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := matchActorArt(dir, actorNames("神木麗", "")); got != want {
			t.Errorf("%s: got %q want %q", sub, got, want)
		}
	}
}

// TestMatchActorArtRejects 不该误命中：非图片扩展名、别的演员。
func TestMatchActorArtRejects(t *testing.T) {
	dir := t.TempDir()
	art := filepath.Join(dir, ".actors")
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"神木麗.txt", "别人.jpg"} {
		if err := os.WriteFile(filepath.Join(art, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := matchActorArt(dir, actorNames("神木麗", "")); got != "" {
		t.Fatalf("不该命中: %q", got)
	}
}
