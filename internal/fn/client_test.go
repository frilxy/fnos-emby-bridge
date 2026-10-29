package fn

import "testing"

func TestQuerySignData(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/v/api/v1/tag/genres?lan=zh-CN", "lan=zh-CN"},
		{"/v/api/v1/tag/list?is_favorite=0&lan=zh-CN&ancestor_guid=abc", "ancestor_guid=abc&is_favorite=0&lan=zh-CN"},
		{"/v/api/v1/search/list?q=%E9%AB%98%E8%BE%BE", "q=高达"},
		{"/v/api/v1/mediadb/list", ""},
	}
	for _, c := range cases {
		if got := querySignData(c.path); got != c.want {
			t.Fatalf("querySignData(%q)=%q,want %q", c.path, got, c.want)
		}
	}
}
