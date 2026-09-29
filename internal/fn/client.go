// Package fn 封装对飞牛影视（FNOS media 服务）私有接口的调用。
// 接口契约来自真机逆向（飞牛影视 0.9.8 / mediasrv 0.8.42，2026-09 实测）：
//   - 业务 token：POST /v/api/v2/user/loginByPassword（密码 SHA256 hex）→ data.token
//     后续请求带 Authorization: <token> 或 Cookie: Trim-MC-token=<token>; mode=relay
//   - Authx 签名（所有 /v/api/v1 请求必须）：
//     sign = MD5(apiKey + "_" + path(不含query) + "_" + nonce + "_" + ts毫秒 + "_" + MD5(body) + "_" + apiSecret)
//     GET 请求 body 为空时 bodyHash = MD5("")
//     header: authx: nonce=<6位>&timestamp=<毫秒>&sign=<md5>
package fn

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// 飞牛影视私有接口的固定 API 密钥（逆向自 fntv-electron / trimemedia-web，真机验证有效）
const (
	apiKey    = "NDzZTVxnRKP8Z0jXg1VAMonaG8akvh"
	apiSecret = "16CCEB3D-AB42-077D-36A1-F355324E4237"
	appName   = "trimemedia-web"
)

// Result 是飞牛 /v/api/v1 所有接口的统一响应包：
//
//	{"code": 0, "msg": "", "data": ...}
type Result struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 是一个飞牛影视接口的薄客户端，封装 token 与 Authx 签名。
type Client struct {
	BaseURL string
	Token   string
	// UserAgent 请求 UA。真机验证：API 端点不校验 UA，但建议模拟客户端。
	UserAgent string
	http      *http.Client

	// OnAuthFail 飞牛侧判定 token 失效时触发（多用户会话层注册，
	// 用于静默重登刷新 token；nil 则忽略）。
	OnAuthFail func()
}

// Clone 派生一个共享底层传输配置、但持有独立 token 的客户端。
// 多用户会话用：每个登录用户一个 clone，互不顶号。
func (c *Client) Clone() *Client {
	return &Client{
		BaseURL:   c.BaseURL,
		UserAgent: c.UserAgent,
		http:      c.http,
	}
}

// NewClient 用 base URL 构造一个客户端。
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		UserAgent: "trimemedia-web",
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// SetToken 设置（或刷新）业务 token。
func (c *Client) SetToken(token string) { c.Token = token }

// Update 热更新服务端地址（admin 后台改连接配置用）。
// 自托管单用户场景飞行中请求极少，直接赋值即可。
func (c *Client) Update(baseURL string) {
	c.BaseURL = strings.TrimRight(baseURL, "/")
}

// md5Hex 求 md5 hex。
func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// sha256Hex 求 SHA256 hex（登录密码哈希）。
func sha256Hex(s string) string {
	return SHA256Hex(s)
}

// SHA256Hex 求 SHA256 hex（导出版，供桥接层密码形态判断用）。
func SHA256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ProbeJSON 调 /v/api/v1/{path} 并返回原始 data 字节（探测/透传未知接口用）。
func (c *Client) ProbeJSON(ctx context.Context, path string, body interface{}) ([]byte, error) {
	res, err := c.do(ctx, "POST", path, body)
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// IsSHA256Hex 判断 s 是否为 64 位十六进制（SHA256 hex）。
func IsSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// authx 构造签名头。path 不含 query；body 为空串时 bodyHash=MD5("")。
// nonce 必须为 6 位且不以 0 开头（真机验证：前导零 nonce 返回 code=5000 invalid sign）。
func authx(path, body string) string {
	nonce := fmt.Sprintf("%06d", rand.Intn(900000)+100000)
	ts := time.Now().UnixMilli()
	bodyHash := md5Hex(body)
	sign := md5Hex(strings.Join([]string{apiKey, path, nonce, fmt.Sprint(ts), bodyHash, apiSecret}, "_"))
	return fmt.Sprintf("nonce=%s&timestamp=%d&sign=%s", nonce, ts, sign)
}

// do 统一发起 /v/api/v1 请求，注入 authx + Authorization/Cookie，解析 Result。
// path 形如 "item/list"、"mediadb/list"（不带前缀，不带 query）。
func (c *Client) do(ctx context.Context, method, path string, body interface{}) (*Result, error) {
	return c.doPath(ctx, method, "/v/api/v1/"+path, body)
}

// doRaw 以原始 JSON 字符串 body 发起请求（POST，参数矩阵实验用）。
func (c *Client) doRaw(ctx context.Context, method, fullPath, rawBody string) (*Result, error) {
	return c.doRawBody(ctx, method, fullPath, rawBody)
}

// doRawBody 原始字符串 body 通用请求。
func (c *Client) doRawBody(ctx context.Context, method, fullPath, rawBody string) (*Result, error) {
	signPath := fullPath
	if i := strings.IndexByte(fullPath, '?'); i >= 0 {
		signPath = fullPath[:i]
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+fullPath, strings.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	req.Header.Set("Authx", authx(signPath, rawBody))
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
		req.Header.Set("Cookie", "Trim-MC-token="+c.Token+"; mode=relay")
	} else {
		req.Header.Set("Cookie", "mode=relay")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("decode result (%d): %s", resp.StatusCode, truncate(string(b), 200))
	}
	if res.Code != 0 {
		return &res, fmt.Errorf("fn error code=%d msg=%s", res.Code, res.Msg)
	}
	return &res, nil
}

// doPath 发起任意路径请求（含 v2 接口），签名路径 = 实际请求路径（不含 query）。
// fullPath 允许带 query（如 episode/list/{guid}?guid=...）——签名时自动剥离，
// 与真机契约一致（authx sign 只拼路径段，见文件头注释）。
func (c *Client) doPath(ctx context.Context, method, fullPath string, body interface{}) (*Result, error) {
	signPath := fullPath
	if i := strings.IndexByte(fullPath, '?'); i >= 0 {
		signPath = fullPath[:i]
	}
	var reader io.Reader
	bodyJSON := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyJSON = string(b)
		reader = strings.NewReader(bodyJSON)
	}

	full := c.BaseURL + fullPath
	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	if bodyJSON != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	req.Header.Set("Authx", authx(signPath, bodyJSON))
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
		// 真机验证：图片等端点认 Cookie（mode=relay），双保险都带上
		req.Header.Set("Cookie", "Trim-MC-token="+c.Token+"; mode=relay")
	} else {
		// 真机验证：无 token 的请求（如登录）也必须带 Cookie 头（值任意），
		// 否则 FN 反代把 /v/api/* 兜底成 SPA HTML
		req.Header.Set("Cookie", "mode=relay")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do %s %s: %w", method, fullPath, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode result (%d): %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if res.Code != 0 {
		// 调试：打印签名要素，定位间歇性 invalid sign
		if res.Code == 5000 {
			log.Printf("[authx-fail] %s %s authx=%s bodyHash=%s resp=%s", method, fullPath,
				req.Header.Get("Authx"), md5Hex(bodyJSON), truncate(string(raw), 120))
		}
		// token 失效：宽匹配（飞牛各版本失效码不统一），触发会话层静默重登
		if c.OnAuthFail != nil && isAuthFailure(res.Code, res.Msg) {
			go c.OnAuthFail()
		}
		return &res, fmt.Errorf("fn error code=%d msg=%s", res.Code, res.Msg)
	}
	return &res, nil
}

// isAuthFailure 判定飞牛响应是否为 token 失效类错误。
// 宽匹配（401 / msg 含 token/登录/权限），误触发代价仅一次防抖重登。
func isAuthFailure(code int, msg string) bool {
	if code == 401 {
		return true
	}
	m := strings.ToLower(msg)
	return strings.Contains(m, "token") || strings.Contains(m, "登录") ||
		strings.Contains(m, "权限") || strings.Contains(m, "auth")
}

// DoRaw 透传一个已构造好的 *http.Request（需自行带好认证头），返回原始响应。
// 用于图片转发等不走 Result 包结构的端点。
func (c *Client) DoRaw(req *http.Request) (*http.Response, error) {
	return c.http.Do(req)
}

// AuthHeaders 返回当前 token 对应的认证头（Cookie 形式，图片端点用）。
func (c *Client) AuthHeaders() map[string]string {
	if c.Token == "" {
		return nil
	}
	return map[string]string{"Cookie": "Trim-MC-token=" + c.Token + "; mode=relay"}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
