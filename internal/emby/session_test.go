package emby_test

// 多用户会话测试（第 24 轮）：
//  1. 两个用户登录 → 各自独立 Emby token，互不顶号
//  2. 同账号重复登录（客户端刷新/重连）→ 复用会话，不触发飞牛真登录（治限流）
//  3. 各自上报播放进度 → 飞牛侧收到各自的账号 token（进度按飞牛账号隔离）
//
// 回归背景：此前全局共用一个飞牛 token，后登录者顶掉先登录者的，
// 所有人进度/收藏/已看全写到最后登录的飞牛账号上（用户真机实测进度互串）。

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

func TestMultiUserSessions(t *testing.T) {
	t.Chdir(t.TempDir()) // bridge-sessions.json 落临时目录，不污染源码树
	mock.MockReset()

	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	defer fnSrv.Close()

	// 全局兜底会话（admin，未匹配请求回落用它）
	client := fn.NewClient(fnSrv.URL)
	_ = client.Login(t.Context(), "admin", fn.SHA256Hex("p"))

	h := emby.NewHandler(client, "fnos-test", "127.0.0.1:8096")
	bridge := httptest.NewServer(h.Handler()) // Handler() 链带 withSession
	defer bridge.Close()

	login := func(user, pass string) string {
		t.Helper()
		resp, err := http.Post(bridge.URL+"/Users/AuthenticateByName", "application/json",
			strings.NewReader(fmt.Sprintf(`{"UserName":%q,"Pwd":%q}`, user, pass)))
		if err != nil {
			t.Fatalf("auth %s: %v", user, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("auth %s status=%d body=%s", user, resp.StatusCode, b)
		}
		var out struct {
			AccessToken string `json:"AccessToken"`
		}
		if err := json.Unmarshal(b, &out); err != nil || out.AccessToken == "" {
			t.Fatalf("auth %s decode: %s", user, b)
		}
		return out.AccessToken
	}
	progress := func(embyToken string, ticks int64) {
		t.Helper()
		req, _ := http.NewRequest("POST", bridge.URL+"/Sessions/Playing/Progress?api_key="+embyToken,
			strings.NewReader(fmt.Sprintf(`{"ItemId":"fv_001","PositionTicks":%d}`, ticks)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("progress: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 204 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("progress status=%d body=%s", resp.StatusCode, b)
		}
	}

	// 1. 两个用户登录：token 独立，各真登录一次
	alice := login("alice", "pa")
	bob := login("bob", "pb")
	if alice == bob {
		t.Fatalf("不同用户拿到同一 token（会话未隔离）")
	}
	if n := mock.MockLoginCount("alice"); n != 1 {
		t.Fatalf("alice 真登录 %d 次，应 1 次", n)
	}
	if n := mock.MockLoginCount("bob"); n != 1 {
		t.Fatalf("bob 真登录 %d 次，应 1 次", n)
	}

	// 2. alice 客户端刷新（重复登录）：复用会话，不触发飞牛登录（限流治理核心）
	if again := login("alice", "pa"); again != alice {
		t.Fatalf("同账号重复登录应复用同一 Emby token")
	}
	if n := mock.MockLoginCount("alice"); n != 1 {
		t.Fatalf("重复登录触发飞牛真登录（%d 次），限流治理失效", n)
	}

	// 3. 进度隔离：alice/bob 各自上报，飞牛侧必须收到各自的账号 token
	progress(alice, 600_000_000) // 60s
	progress(bob, 1200_000_000)  // 120s
	recs := mock.MockRecords()
	if len(recs) != 2 {
		t.Fatalf("play/record %d 条，应 2 条", len(recs))
	}
	byTok := map[string]int64{}
	for _, r := range recs {
		byTok[r.Token] = r.Ts
	}
	if byTok["MOCKTOKEN_alice_1"] != 60 {
		t.Fatalf("alice 的进度未按其飞牛会话记录: %v", byTok)
	}
	if byTok["MOCKTOKEN_bob_1"] != 120 {
		t.Fatalf("bob 的进度未按其飞牛会话记录: %v", byTok)
	}
}
