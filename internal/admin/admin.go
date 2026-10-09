// Package admin 提供桥接后台管理：/admin 单页 UI + 配置 API。
// 当前能力：自定义媒体库图片（URL 覆盖库卡片海报，客户端请求库 Primary 图时 302）。
// 配置持久化为 JSON（CONFIG_PATH，默认工作目录 bridge-config.json），保存即热生效。
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 上传/读取白名单：guid 是 32 位 hex；扩展名限常见图片格式。
var (
	guidRe    = regexp.MustCompile(`^[0-9a-fA-F]{8,64}$`)
	imgNameRe = regexp.MustCompile(`^[0-9a-f]{8,64}\.(jpg|jpeg|png|webp|gif)$`)
	imgExtRe  = regexp.MustCompile(`^\.(jpg|jpeg|png|webp|gif)$`)
)

// ConnCfg 飞牛连接配置（落盘为明文密码——自托管内网/信任环境使用）。
// 空字段 = 回退环境变量默认值。
type ConnCfg struct {
	Base string `json:"base"`
	User string `json:"user"`
	Pass string `json:"pass,omitempty"`
}

// Config 落盘配置结构。扩展新配置项时往这里加字段（JSON 向后兼容）。
type Config struct {
	Connection    *ConnCfg          `json:"connection,omitempty"`
	LibraryImages map[string]string `json:"library_images"` // 库 guid -> 自定义图片 URL
	AdminPath     string            `json:"admin_path,omitempty"`
}

// Store 配置存储：内存态 + 文件持久化，读写锁保护。
type Store struct {
	mu       sync.RWMutex
	path     string
	cfg      Config
	auth     adminAuth
	inner    http.Handler
	authPath string
}

type adminAuth struct {
	user    string
	pass    string
	token   string
	expires time.Time
}

// SetAdminCredentials 注入后台登录账号（环境变量配置；与飞牛登录账号解耦）。
func (s *Store) SetAdminCredentials(user, pass string) {
	if user == "" || pass == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth.user = user
	s.auth.pass = pass
}

// LoadStore 从 path 加载配置；文件不存在/损坏时用空配置起（不致命）。
func LoadStore(path string) *Store {
	s := &Store{path: path, cfg: Config{LibraryImages: map[string]string{}}}
	b, err := os.ReadFile(path)
	if err == nil {
		var c Config
		if json.Unmarshal(b, &c) == nil {
			if c.LibraryImages == nil {
				c.LibraryImages = map[string]string{}
			}
			s.cfg = c
		}
	}
	return s
}

// Connection 读当前连接配置（无则 nil）。
func (s *Store) Connection() *ConnCfg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg.Connection == nil {
		return nil
	}
	c := *s.cfg.Connection
	return &c
}

// SetConnection 保存连接配置并落盘。
func (s *Store) SetConnection(base, user, pass string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Connection = &ConnCfg{Base: base, User: user, Pass: pass}
	return s.saveLocked()
}

// AdminPath 返回后台安全路径，默认 /admin。
func (s *Store) AdminPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg.AdminPath == "" {
		return "/admin"
	}
	return s.cfg.AdminPath
}

// SetAdminPath 保存后台安全路径并落盘（1-64 字符，斜杠开头，不能以斜杠结尾）。
func (s *Store) SetAdminPath(path string) error {
	path = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(path), "/"), "/")
	if path == "" {
		path = "/admin"
	}
	if !strings.HasPrefix(path, "/") || len(path) > 64 || strings.Contains(path, "//") {
		return fmt.Errorf("安全路径必须以 / 开头，长度 1-64，且不能出现连续斜杠")
	}
	for _, part := range strings.Split(path[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("安全路径包含非法段：%q", part)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.AdminPath = path
	s.inner = nil
	s.authPath = path
	return s.saveLocked()
}

// LibraryImage 查库自定义图 URL（无则空串）。图片端点每次请求都查，必须无锁快速。
func (s *Store) LibraryImage(guid string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.LibraryImages[guid]
}

// SetLibraryImage 设置/清除（url 空 = 清除）库自定义图并落盘。
func (s *Store) SetLibraryImage(guid, url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.LibraryImages == nil {
		s.cfg.LibraryImages = map[string]string{}
	}
	if url == "" {
		delete(s.cfg.LibraryImages, guid)
	} else {
		s.cfg.LibraryImages[guid] = url
	}
	return s.saveLocked()
}

// saveLocked 落盘（调用方须持写锁）：临时文件 + 原子替换，避免写一半损坏。
func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// LibInfo 后台库列表条目。
type LibInfo struct {
	Guid  string `json:"guid"`
	Name  string `json:"name"`
	Total int    `json:"total"`
	Image string `json:"image"` // 当前自定义图（空=未设置）
}

// ConnInfo 连接状态（GET 回显；密码不回传只报有无）。
type ConnInfo struct {
	Base    string `json:"base"`
	User    string `json:"user"`
	PassSet bool   `json:"pass_set"`
	OK      bool   `json:"ok"`    // 当前飞牛连接是否正常（token 非空）
	Token   string `json:"token"` // token 前 8 位展示用
}

// ConnDeps 连接配置的依赖注入（main 装配：Apply 须先试登录成功再切换+落盘）。
type ConnDeps struct {
	Get   func() ConnInfo
	Apply func(base, user, pass string) error
}

// imagesDir 本地图片目录（配置文件同目录 bridge-images/）。
func (s *Store) imagesDir() string {
	return filepath.Join(filepath.Dir(s.path), "bridge-images")
}

// serveImage GET /admin/images/{name}：回本地保存的库图。
// name 白名单校验（guid hex + 受支持扩展名），防目录穿越。
func (s *Store) serveImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !imgNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache") // 换图立即生效
	http.ServeFile(w, r, filepath.Join(s.imagesDir(), name))
}

// uploadImage POST /admin/api/lib-image/upload（multipart：guid + file）。
// 保存到本地 bridge-images/{guid}.{ext}，并把库图配置指向本地路径。
func (s *Store) uploadImage(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 20 << 20 // 20MB
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		http.Error(w, "文件过大（上限 20MB）", http.StatusBadRequest)
		return
	}
	guid := r.FormValue("guid")
	if guid == "" || !guidRe.MatchString(guid) {
		http.Error(w, "bad guid", http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少文件", http.StatusBadRequest)
		return
	}
	defer f.Close()
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !imgExtRe.MatchString(ext) {
		http.Error(w, "仅支持 jpg/png/webp/gif", http.StatusBadRequest)
		return
	}
	// 嗅探真实内容类型（防伪装扩展名）
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ct := http.DetectContentType(head[:n])
	if !strings.HasPrefix(ct, "image/") {
		http.Error(w, "不是图片文件", http.StatusBadRequest)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dir := s.imagesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 清掉同 guid 旧文件（可能扩展名不同）
	if olds, _ := filepath.Glob(filepath.Join(dir, guid+".*")); len(olds) > 0 {
		for _, old := range olds {
			_ = os.Remove(old)
		}
	}
	dst := filepath.Join(dir, guid+ext)
	out, err := os.Create(dst)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		_ = os.Remove(dst)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := out.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 配置指向本地路径（客户端 302 到桥接自身）
	imageURL := s.AdminPath() + "/images/" + guid + ext
	if err := s.SetLibraryImage(guid, imageURL); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "url": imageURL})
}

// Handler 返回 /admin 页面 + API。listLibs 由 main 注入（依赖 fn 客户端列库）。
func (s *Store) Handler(listLibs func() []LibInfo, conn *ConnDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		basePath := s.AdminPath()
		if r.URL.Path != basePath && !strings.HasPrefix(r.URL.Path, basePath+"/") {
			http.NotFound(w, r)
			return
		}
		oldPath := r.URL.Path
		r.URL.Path = strings.TrimPrefix(r.URL.Path, basePath)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		s.innerHandler(listLibs, conn).ServeHTTP(w, r)
		r.URL.Path = oldPath
	})
}

func (s *Store) innerHandler(listLibs func() []LibInfo, conn *ConnDeps) http.Handler {
	s.mu.RLock()
	handler := s.inner
	s.mu.RUnlock()
	if handler != nil {
		return handler
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /", s.page)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /images/{name}", s.serveImage)
	mux.Handle("POST /api/lib-image/upload", s.protected(http.HandlerFunc(s.uploadImage)))
	mux.Handle("GET /api/libs", s.protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		libs := listLibs()
		for i := range libs {
			libs[i].Image = s.LibraryImage(libs[i].Guid)
		}
		writeJSON(w, libs)
	})))
	mux.Handle("POST /api/lib-image", s.protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Guid string `json:"guid"`
			URL  string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Guid == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.SetLibraryImage(req.Guid, req.URL); err != nil {
			log.Printf("[admin] 保存库图片配置: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})))
	if conn != nil {
		mux.Handle("GET /api/connection", s.protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, conn.Get())
		})))
		// 保存并重连：Apply 内部先试登录，成功才切换+落盘；失败返回 400+原因
		mux.Handle("POST /api/connection", s.protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Base string `json:"base"`
				User string `json:"user"`
				Pass string `json:"pass"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Base == "" || req.User == "" {
				http.Error(w, "地址与账号不能为空", http.StatusBadRequest)
				return
			}
			if err := conn.Apply(req.Base, req.User, req.Pass); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		})))
	}
	mux.Handle("GET /api/security", s.protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"admin_path": s.AdminPath()})
	})))
	mux.Handle("POST /api/security", s.protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.SetAdminPath(req.Path); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "path": s.AdminPath()})
	})))

	s.mu.Lock()
	s.inner = mux
	s.mu.Unlock()
	return mux
}

func (s *Store) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(AdminHTML()))
}

func (s *Store) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	user, pass := s.auth.user, s.auth.pass
	s.mu.RUnlock()
	userOK := subtle.ConstantTimeCompare([]byte(req.User), []byte(user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(req.Pass), []byte(pass)) == 1
	if user == "" || pass == "" || !userOK || !passOK {
		http.Error(w, "账号或密码错误", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	if s.auth.token == "" || time.Now().After(s.auth.expires) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err == nil {
			s.auth.token = fmt.Sprintf("%x", b)
			s.auth.expires = time.Now().Add(12 * time.Hour)
		}
	}
	token := s.auth.token
	expires := s.auth.expires
	s.mu.Unlock()
	if token == "" {
		http.Error(w, "生成登录态失败", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "bridge_admin", Value: token, Path: "/", Expires: expires, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Store) logout(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.auth.token, s.auth.expires = "", time.Time{}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "bridge_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Store) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": s.authorized(r)})
}

func (s *Store) authorized(r *http.Request) bool {
	cookie, err := r.Cookie("bridge_admin")
	if err != nil || cookie.Value == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auth.token != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.auth.token)) == 1 && time.Now().Before(s.auth.expires)
}

func (s *Store) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "未登录", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
