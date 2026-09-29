package emby

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"fnos-emby-bridge/internal/fn"
)

// 复刻 fntv-electron 云盘代理行为（playvideo.go + cloud.go）。
// 云盘直链按 cloud_type 分流：
//   - 115（type=3）：全局限速 1 req/s
//   - 夸克（type=4）：10MiB 固定分块 + 串行（1 并发）边下边播，防风控
//   - 其它（阿里/百度/123）：透明代理透传 + 302 跟随
const (
	chunkSize            = 10 * 1024 * 1024 // 10MiB 固定分块
	maxConcurrentChunks  = 1                // 串行，防网盘风控
	cloudRedirectMaxHops = 3                // 云盘直链 302 最多跟随跳数
)

// 云盘类型常量（飞牛）：1 百度 2 阿里 3 115 4 夸克 5 123
const (
	cloud115Pan = 3
	cloudQuark  = 4
)

// cloudFileCache 按直链 URL 缓存文件大小（探测一次，复用）。
var cloudFileCache struct {
	mu   sync.Mutex
	refs map[string]*int64
}

func init() {
	cloudFileCache.refs = make(map[string]*int64)
}

func cachedFileSize(url string) int64 {
	cloudFileCache.mu.Lock()
	defer cloudFileCache.mu.Unlock()
	if v, ok := cloudFileCache.refs[url]; ok {
		return *v
	}
	return -1
}

func storeFileSize(url string, size int64) {
	cloudFileCache.mu.Lock()
	defer cloudFileCache.mu.Unlock()
	cloudFileCache.refs[url] = &size
}

// probeFileSize 用 bytes=0-0 探测总大小（fntv-electron getMetaInfo 同款）。
func probeFileSize(ctx context.Context, client *http.Client, url string, headers map[string]string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("probe: unexpected status %d", resp.StatusCode)
	}
	cr := resp.Header.Get("Content-Range") // bytes 0-0/total
	idx := strings.Index(cr, "/")
	if idx < 0 || idx+1 >= len(cr) {
		return 0, fmt.Errorf("probe: bad Content-Range %q", cr)
	}
	total, err := strconv.ParseInt(cr[idx+1:], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("probe: parse total %q", cr[idx+1:])
	}
	return total, nil
}

// serveCloudDirect 处理云盘直链请求：按 cloud_type 选择 115 限速 / 夸克分块 / 透明代理。
func (h *Handler) serveCloudDirect(w http.ResponseWriter, r *http.Request, direct *fn.DirectLinkInfo) {
	client := h.cloudClient()

	// 拼装上游请求头：cookie + UA（来自飞牛 stream 响应 header，非写死 trim_player）
	upHeaders := map[string]string{}
	if len(direct.Cookies) > 0 {
		upHeaders["Cookie"] = strings.Join(direct.Cookies, "; ")
	}
	if len(direct.UserAgent) > 0 {
		upHeaders["User-Agent"] = direct.UserAgent[0]
	}

	// 115：全局 1 req/s 限速
	if direct.CloudType == cloud115Pan {
		h.lim115.Wait(r.Context())
	}

	// 夸克：10MiB 固定分块 + 串行边下边播（fntv-electron ChunkedProxy）
	if direct.CloudType == cloudQuark {
		h.serveChunked(w, r, direct, client, upHeaders)
		return
	}

	// 其它云盘（阿里/百度/123）：透明代理 + 302 跟随
	h.serveTransparent(w, r, direct, client, upHeaders)
}

// serveChunked 夸克网盘"边下边播"：按 10MiB 固定分块请求上游，串行回写。
// 复刻 fntv-electron cloud.go serveMPVRangeSimple。
func (h *Handler) serveChunked(w http.ResponseWriter, r *http.Request, direct *fn.DirectLinkInfo, client *http.Client, upHeaders map[string]string) {
	// 1) 探测总大小（先查缓存）；探测失败（上游不给 206，常见于风控）退化为透明透传
	size := cachedFileSize(direct.URL)
	if size < 0 {
		probed, err := probeFileSize(r.Context(), client, direct.URL, upHeaders)
		if err != nil {
			h.serveTransparent(w, r, direct, client, upHeaders)
			return
		}
		size = probed
		storeFileSize(direct.URL, size)
	}

	// 2) 解析客户端 Range
	start, end, ok := parseClientRange(r.Header.Get("Range"), size)
	if !ok || start >= size {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// 3) 客户端请求的分块内，串行取上游 10MiB 子块并流式回写
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusPartialContent)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 串行搬运 [start, end]，每次上游请求 10MiB
	for off := start; off <= end; {
		chunkEnd := off + chunkSize - 1
		if chunkEnd > end {
			chunkEnd = end
		}
		if !h.serveChunk(w, r, direct, client, upHeaders, off, chunkEnd) {
			return
		}
		off = chunkEnd + 1
	}
}

// serveChunk 取上游 [start,end] 一段，流式写到 w。客户端断开时返回 false 终止。
func (h *Handler) serveChunk(w http.ResponseWriter, r *http.Request, direct *fn.DirectLinkInfo, client *http.Client, upHeaders map[string]string, start, end int64) bool {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, direct.URL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return false
	}
	for k, v := range upHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := client.Do(req)
	if err != nil {
		// 客户端断开（context canceled）或上游瞬断
		if r.Context().Err() != nil {
			return false
		}
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return false
	}
	defer resp.Body.Close()

	// 200（忽略 Range）时按请求偏移裁剪：先丢 start 字节，再只拷贝需要长度
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
	default:
		http.Error(w, fmt.Sprintf("upstream status %d", resp.StatusCode), http.StatusBadGateway)
		return false
	}

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if resp.StatusCode == http.StatusOK {
		// 上游无视 Range 返回全量：丢弃 [0, start) 后只写 want 字节
		if _, err = io.CopyN(io.Discard, resp.Body, start); err != nil {
			return false
		}
		_, err = io.CopyN(w, resp.Body, end-start+1)
	} else {
		_, err = io.Copy(w, resp.Body)
	}
	if err != nil {
		if r.Context().Err() != nil {
			return false // 客户端断开
		}
		// 透传错误
		return false
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return true
}

// serveTransparent 阿里/百度/123 云盘透明代理：带 Range 直连上游，302 最多 3 跳跟随。
func (h *Handler) serveTransparent(w http.ResponseWriter, r *http.Request, direct *fn.DirectLinkInfo, client *http.Client, upHeaders map[string]string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, direct.URL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, v := range upHeaders {
		req.Header.Set(k, v)
	}
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}

	for i := 0; i < cloudRedirectMaxHops; i++ {
		resp, err := client.Do(req)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				http.Error(w, "302 with no Location", http.StatusBadGateway)
				return
			}
			if strings.HasPrefix(loc, "/") {
				loc = req.URL.Scheme + "://" + req.URL.Host + loc
			}
			req, err = http.NewRequestWithContext(r.Context(), http.MethodGet, loc, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			for k, v := range upHeaders {
				req.Header.Set(k, v)
			}
			if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
				req.Header.Set("Range", rangeHdr)
			}
			continue
		}
		for _, k := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		resp.Body.Close()
		return
	}
	http.Error(w, "too many redirects", http.StatusBadGateway)
}

// cloudClient 返回流式 http.Client（Timeout=0 适配长连接）。
func (h *Handler) cloudClient() *http.Client {
	if h.cloudHTTP == nil {
		h.cloudHTTP = &http.Client{Timeout: 0}
	}
	return h.cloudHTTP
}

// parseClientRange 解析 "bytes=start-end"，无 Range 返回 0/size-1。
// end==-1 表示到文件尾。返回 (start, end, ok)。
func parseClientRange(rangeHdr string, size int64) (int64, int64, bool) {
	if rangeHdr == "" {
		return 0, size - 1, true
	}
	if !strings.HasPrefix(rangeHdr, "bytes=") {
		return 0, -1, false
	}
	spec := strings.TrimPrefix(rangeHdr, "bytes=")
	// 取第一个 range 段（逗号分隔时只处理第一个）
	if comma := strings.Index(spec, ","); comma >= 0 {
		spec = spec[:comma]
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, -1, false
	}
	if parts[0] == "" {
		// suffix form: -N 表示末尾 N 字节
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, -1, false
		}
		if n >= size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 {
		return 0, -1, false
	}
	if parts[1] == "" {
		return start, size - 1, true
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return start, size - 1, true // 越界 end 按到尾处理
	}
	if end > size-1 {
		end = size - 1
	}
	return start, end, true
}
