// fnos-emby-bridge 是一个"假 Emby 服务端"：
// 前端讲 Emby 协议给 Emby 官方播放器，后端调飞牛影视 /v/api/v1 私有接口。
//
// 配置（环境变量）：
//
//	FNOS_BASE   飞牛影视服务端 base URL，如 http://192.168.1.10:5666
//	FNOS_USER   飞牛影视登录账号
//	FNOS_PASS   飞牛影视登录密码
//	PORT        桥接服务监听端口，默认 8096
//	HOST        客户端访问本桥接的主机:端口（用于拼绝对媒体/海报 URL），默认 127.0.0.1:PORT
//	SERVER_NAME 显示的服务器名，默认 fnos
//	ADMIN_USER /admin 后台账号，默认 admin
//	ADMIN_PASS /admin 后台密码，默认 admin123
//
// 0.9.8 起媒体库通过 mediadb/list 自动列出，无需 SEED_GUIDS。
//
// 构建：
//
//	go build -o fnos-emby-bridge ./cmd/fnos-emby-bridge
//
// 运行：
//
//	FNOS_BASE=http://nas:5666 FNOS_USER=admin FNOS_PASS=xx PORT=8096 ./fnos-emby-bridge
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"fnos-emby-bridge/internal/admin"
	"fnos-emby-bridge/internal/emby"
	"fnos-emby-bridge/internal/fn"
)

func main() {
	// 环境变量只是默认值/引导；admin 后台保存过的连接配置（bridge-config.json）优先
	base := os.Getenv("FNOS_BASE")
	user := os.Getenv("FNOS_USER")
	pass := os.Getenv("FNOS_PASS")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8096"
	}
	host := os.Getenv("HOST")
	if host == "" {
		host = "127.0.0.1:" + port
	}
	name := os.Getenv("SERVER_NAME")
	if name == "" {
		name = "fnos"
	}

	// 配置存储（连接配置 + 库自定义图，落盘 JSON 热生效）
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "bridge-config.json"
	}
	store := admin.LoadStore(cfgPath)
	adminUser := os.Getenv("ADMIN_USER")
	adminPass := os.Getenv("ADMIN_PASS")
	if adminUser == "" {
		adminUser = "admin"
	}
	if adminPass == "" {
		adminPass = "admin123"
	}
	store.SetAdminCredentials(adminUser, adminPass)
	if cc := store.Connection(); cc != nil {
		if cc.Base != "" {
			base = cc.Base
		}
		if cc.User != "" {
			user = cc.User
		}
		if cc.Pass != "" {
			pass = cc.Pass
		}
	}
	if base == "" || user == "" {
		log.Printf("飞牛连接未配置，请打开后台填写")
	}

	// 先用账号登录拿 token
	client := fn.NewClient(base)
	if base != "" && user != "" && pass != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.Login(ctx, user, fn.SHA256Hex(pass)); err != nil {
			log.Printf("飞牛登录失败：%v（可在后台修改连接）", err)
		} else {
			log.Printf("飞牛登录成功，token=%s...", client.Token[:8])
		}
	}

	h := emby.NewHandler(client, name, host)
	h.SetLibImageResolver(store.LibraryImage)

	// 后台管理：/admin（UI + 配置 API）
	adminHandler := store.Handler(func() []admin.LibInfo {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		dbs, err := client.MediaDBList(ctx)
		if err != nil {
			log.Printf("[admin] 列媒体库: %v", err)
			return nil
		}
		libs := make([]admin.LibInfo, 0, len(dbs))
		for _, db := range dbs {
			li := admin.LibInfo{Guid: db.Guid, Name: db.Title}
			if _, total, err := client.ItemListPage(ctx, "ancestor_guid", db.Guid, 1, 1); err == nil {
				li.Total = total
			}
			libs = append(libs, li)
		}
		return libs
	}, &admin.ConnDeps{
		Get: func() admin.ConnInfo {
			cc := store.Connection()
			info := admin.ConnInfo{
				Base: base, User: user,
				PassSet: pass != "",
				OK:      client.Token != "",
				Token:   client.Token[:8],
			}
			if cc != nil {
				if cc.Base != "" {
					info.Base = cc.Base
				}
				if cc.User != "" {
					info.User = cc.User
				}
				info.PassSet = info.PassSet || cc.Pass != ""
			}
			return info
		},
		Apply: func(newBase, newUser, newPass string) error {
			// 密码留空 = 沿用已存/当前密码
			effPass := newPass
			if effPass == "" {
				effPass = pass
			}
			// 先试登录（不污染现有连接），成功才切换+落盘
			test := fn.NewClient(newBase)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := test.Login(ctx, newUser, fn.SHA256Hex(effPass)); err != nil {
				return fmt.Errorf("连接失败：%w", err)
			}
			client.Update(newBase)
			client.SetToken(test.Token)
			base, user, pass = newBase, newUser, effPass
			if err := store.SetConnection(newBase, newUser, effPass); err != nil {
				log.Printf("[admin] 连接配置落盘: %v", err)
			}
			log.Printf("[admin] 飞牛连接已切换：%s（token=%s...）", newBase, test.Token[:8])
			return nil
		},
	})

	mux := http.NewServeMux()
	mux.Handle("/", withCORS(h.Handler())) // Handler 已带 /emby 前缀兼容

	// rootPathFix 最外层就地清洗非规范路径（重复斜杠/尾斜杠）。
	// 必须在 ServeMux 之前做：Go 标准库 mux 对 ///emby//x 这类路径会 301，
	// OkHttp 跟 301 时 POST 降级为 GET → Yamby/AfuseKt 登录必败（go-emby
	// 同款问题为 AfuseKt 专门做过回归）。清洗后直接命中路由，不发 301。
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.Contains(p, "//") {
			for strings.Contains(p, "//") {
				p = strings.ReplaceAll(p, "//", "/")
			}
		}
		if p != "/" {
			p = strings.TrimRight(p, "/")
			if p == "" {
				p = "/"
			}
		}
		if p != r.URL.Path {
			r.URL.Path = p
		}
		if p == store.AdminPath() || strings.HasPrefix(p, store.AdminPath()+"/") {
			adminHandler.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: root,
	}

	log.Printf("后台管理：http://<host>:%s/admin", port)
	// 优雅退出
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("收到退出信号，关闭服务...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("FNOS-EFBy-Bridge 启动，监听 :%s，飞牛 base=%s", port, base)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务启动失败：%v", err)
	}
}

// withCORS 给所有响应加 CORS 头，方便浏览器/网页版 Emby 客户端跨域访问。
func withCORS(next http.Handler) http.Handler {
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
