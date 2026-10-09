package emby

import "testing"

// fork 新增工具函数的单元测试（package emby 内部测试，可访问未导出符号）。

func TestMimeForExt(t *testing.T) {
	cases := map[string]string{
		"mp4": "video/mp4", ".MP4": "video/mp4",
		"mkv": "video/x-matroska", ".mkv": "video/x-matroska",
		"webm": "video/webm", "mov": "video/quicktime",
		"ts": "video/mp2t", "avi": "video/x-msvideo",
		"": "", ".": "", "exe": "",
	}
	for in, want := range cases {
		if got := mimeForExt(in); got != want {
			t.Errorf("mimeForExt(%q)=%q want %q", in, got, want)
		}
	}
}

func TestGenericMime(t *testing.T) {
	generic := []string{"", "  ", "application/octet-stream",
		"application/octet-stream; charset=utf-8", "binary/octet-stream"}
	for _, ct := range generic {
		if !genericMime(ct) {
			t.Errorf("genericMime(%q) 应为 true", ct)
		}
	}
	specific := []string{"video/mp4", "video/x-matroska; codecs=h264", "image/png"}
	for _, ct := range specific {
		if genericMime(ct) {
			t.Errorf("genericMime(%q) 应为 false", ct)
		}
	}
}

func TestParseTotalSize(t *testing.T) {
	cases := map[string]int64{
		"bytes 0-0/6721466674": 6721466674,
		"bytes 100-199/4096":   4096,
		"bytes */4096":         4096,
		"":                     0,
		"garbage":              0,
		"bytes 0-0/0":          0,
		"bytes 0-0/abc":        0,
	}
	for in, want := range cases {
		if got := parseTotalSize(in); got != want {
			t.Errorf("parseTotalSize(%q)=%d want %d", in, got, want)
		}
	}
}

func TestStripAbsoluteURLPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// 实测形态：Go 已把 "http://" 归一成 "http:/"
		{"/embyhttp:/192.168.10.229:8096/Videos/x/Stream", "/Videos/x/Stream", true},
		// 双斜杠形态（部分 HTTP 客户端/代理会保留）
		{"/embyhttp://192.168.10.229:8096/Videos/x/stream", "/Videos/x/stream", true},
		{"/http:/host:8096/Videos/x/stream", "/Videos/x/stream", true},
		{"http:/host/Videos/x/stream", "/Videos/x/stream", true},
		{"/embyhttps://nas.local/Videos/x/stream", "/Videos/x/stream", true},
		// 畸形路径里再嵌一层 /emby 也要能还原（交给后续 stripEmbyPrefix 处理）
		{"/embyhttp:/host/emby/Videos/x/stream", "/emby/Videos/x/stream", true},
		// 正常路径不得被改动
		{"/Videos/x/stream", "/Videos/x/stream", false},
		{"/emby/Videos/x/stream", "/emby/Videos/x/stream", false},
		{"/System/Info/Public", "/System/Info/Public", false},
		{"/", "/", false},
		// 只有 host 没有路径 → 不改
		{"/embyhttp:/192.168.10.229:8096", "/embyhttp:/192.168.10.229:8096", false},
	}
	for _, c := range cases {
		got, ok := stripAbsoluteURLPrefix(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("stripAbsoluteURLPrefix(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAudioDisplayTitle(t *testing.T) {
	// 默认开启：手机普遍无法直解的编码会被标注
	hostile := []string{"dts", "DTS-HD", "truehd", "flac", "alac", "pcm_s24le"}
	for _, c := range hostile {
		got := audioDisplayTitle("国语 "+c, c)
		if got == "国语 "+c {
			t.Errorf("codec %q 应被标注，实际未变: %q", c, got)
		}
	}
	// 广泛支持的编码不标注
	for _, c := range []string{"aac", "mp3", "ac3", "eac3", "opus", "vorbis"} {
		base := "国语 " + c
		if got := audioDisplayTitle(base, c); got != base {
			t.Errorf("codec %q 不应被标注，实际: %q", c, got)
		}
	}
	// 空标题不应产生以分隔符开头的怪串
	if got := audioDisplayTitle("", "truehd"); got != "需软解" {
		t.Errorf("空标题 result=%q", got)
	}
	// 重复调用不叠加
	once := audioDisplayTitle("国语", "dts")
	if twice := audioDisplayTitle(once, "dts"); twice != once {
		t.Errorf("重复标注: %q -> %q", once, twice)
	}
	// 可用环境变量关闭
	t.Setenv("AUDIO_TRACK_HINT", "0")
	if got := audioDisplayTitle("国语", "dts"); got != "国语" {
		t.Errorf("AUDIO_TRACK_HINT=0 时应保持原样，实际 %q", got)
	}
}
