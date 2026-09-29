// verify 是本地联调启动器：同时起 mock 飞牛 + 桥接，供 curl 模拟 Emby 官方客户端请求序列。
// 用法：go run ./cmd/verify  （mock 飞牛随机端口，桥接 :8096）
package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"fnos-emby-bridge/internal/emby"
	"fnos-emby-bridge/internal/fn"
	"fnos-emby-bridge/internal/mock"
)

func main() {
	// 1) mock 飞牛（本地文件模式，走 media/range）
	fnSrv := httptest.NewServer(mock.NewHandler(false, ""))
	defer fnSrv.Close()
	log.Printf("mock 飞牛: %s", fnSrv.URL)

	// 2) 桥接（与 main 相同的装配方式）
	client := fn.NewClient(fnSrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Login(ctx, "admin", fn.SHA256Hex("pass")); err != nil {
		log.Fatalf("mock 飞牛登录失败: %v", err)
	}
	h := emby.NewHandler(client, "fnos", "127.0.0.1:8096")

	log.Printf("桥接监听 :8096（/emby 前缀 + CORS 已开）")
	log.Fatal(http.ListenAndServe(":8096", cors(h.Handler())))
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
