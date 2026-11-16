package emby

// 影视库封面：飞牛原生会把库内 3 张海报拼成一张库封面。
// 这里在桥接侧复刻：从库里稳定随机挑 3 个有 poster 的条目，取飞牛 sys/img，
// 按竖版三栏拼接成 600x900 JPEG；按 30 分钟桶缓存，ImageTags.Primary 同步换 tag，
// 客户端会重新拉取。

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"fnos-emby-bridge/internal/fn"
)

const (
	// 影视库卡片是横向的；画布按卡片比例全幅铺满，三张海报各占 1/3，
	// 只做轻微居中裁剪，避免黑边和之前的极端放大。
	collageWidth         = 1200
	collageHeight        = 720
	collageLayoutVersion = "v5"
	// collageTTL 只是内存里缓存的兜底有效期：真正的「要不要重生」由内容签名决定，
	// 内容不变就一直复用同一张封面。
	collageTTL = 6 * time.Hour
	// collageSigTTL 内容签名（条目数 + 最新入库时间）的缓存时长。
	collageSigTTL = 5 * time.Minute
)

// collageSig 库内容签名：条目总数 + 最新入库时间。
//
// 以前用的是「30 分钟时间桶」——没新片也会每半小时重做一次封面，客户端每半小时
// 重下一次图。改成内容签名后，只有库里的条目增减才会换图。
// 签名本身缓存 5 分钟，避免每次 /UserViews 都去列一遍库。
func (h *Handler) collageSig(ctx context.Context, client *fn.Client, libID string) string {
	key := "collagesig:" + client.Token + ":" + libID
	v, err := h.cache.Do(key, collageSigTTL, func() (any, error) {
		list, total, err := client.ItemListPage(ctx, "ancestor_guid", libID, 1, 1)
		if err != nil {
			return "", err
		}
		var latest int64
		for _, it := range list {
			if t := it.CreatedAt(); t > latest {
				latest = t
			}
		}
		return fmt.Sprintf("%d-%d", total, latest), nil
	})
	if err != nil {
		return collageLayoutVersion
	}
	if s, _ := v.(string); s != "" {
		return s
	}
	return collageLayoutVersion
}

// collageTag 给 MediaDB 的 PrimaryImageTag：由「库 guid + 内容签名 + 布局版本」决定，
// 库内容不变就恒定，客户端不会重复下载。
func collageTag(guid, sig string) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%s|%s", guid, sig, collageLayoutVersion)
	return fmt.Sprintf("c%x", h.Sum64())
}

// libraryCollage 生成/缓存库封面。
func (h *Handler) libraryCollage(ctx context.Context, client *fn.Client, libID string) ([]byte, error) {
	sig := h.collageSig(ctx, client, libID)
	key := fmt.Sprintf("collage:%s:%s:%s", client.Token, libID, sig)
	v, err := h.cache.Do(key, collageTTL, func() (any, error) {
		list, err := h.typedItems(ctx, client, libID, []string{"Movie", "TV", "Video", "Directory"}, 0)
		if err != nil {
			return nil, err
		}
		picks := pickCollageItems(list, libID, seedFromSig(sig))
		if len(picks) == 0 {
			return nil, fmt.Errorf("库 %s 没有可用的海报", libID)
		}
		imgs := make([]image.Image, 0, len(picks))
		for _, pick := range picks {
			p := pick.path
			img, err := h.fetchCollageImage(ctx, client, p)
			if err != nil {
				log.Printf("[collage] 库 %s 取图 %q 失败: %v", libID, p, err)
				continue
			}
			imgs = append(imgs, img)
		}
		if len(imgs) == 0 {
			return nil, fmt.Errorf("库 %s 的图片全部获取失败", libID)
		}
		data, err := composeCollage(imgs)
		if err != nil {
			return nil, err
		}
		log.Printf("[collage] 库 %s 生成封面：%d 张海报，%d 字节", libID, len(imgs), len(data))
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	data, _ := v.([]byte)
	if len(data) == 0 {
		return nil, fmt.Errorf("库 %s 封面为空", libID)
	}
	return data, nil
}

// collagePick 选中的海报：条目 + 实际要取的那张图路径。
type collagePick struct {
	item fn.MediaItem
	path string
}

// seedFromSig 把内容签名折成一个稳定的随机种子（内容变了才换海报组合）。
func seedFromSig(sig string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sig))
	return int64(h.Sum64())
}

// pickCollageItems 用库 guid + 内容签名做种子，稳定随机挑最多 3 张**互不相同**的海报。
//
// 只按条目去重不够：同一次刮削经常让多个条目（剧集与它的季、同一部的多个版本、
// 目录容器与里面的视频）共用同一个 poster 路径，按条目挑 3 个会拼出 3 张一模一样的图。
// 这里按「实际取图路径」去重，保证三栏是三张不同的海报。
func pickCollageItems(list []fn.MediaItem, libID string, seed int64) []collagePick {
	seenPath := make(map[string]struct{}, len(list))
	seenItem := make(map[string]struct{}, len(list))
	candidates := make([]collagePick, 0, len(list))
	for _, m := range list {
		if _, dup := seenItem[m.Guid]; dup {
			continue
		}
		p := m.PosterPath()
		if p == "" {
			p = m.BackdropPath()
		}
		if p == "" {
			continue
		}
		if _, dup := seenPath[p]; dup {
			continue
		}
		seenItem[m.Guid] = struct{}{}
		seenPath[p] = struct{}{}
		candidates = append(candidates, collagePick{item: m, path: p})
	}
	if len(candidates) == 0 {
		return nil
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%d", libID, seed)
	rnd := rand.New(rand.NewSource(int64(h.Sum64())))
	rnd.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	return candidates
}

// fetchCollageImage 取飞牛图片并解码（支持 jpeg/png/webp）。
func (h *Handler) fetchCollageImage(ctx context.Context, client *fn.Client, p string) (image.Image, error) {
	var req *http.Request
	var err error
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, p, nil)
	} else {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet,
			client.BaseURL+"/v/api/v1/sys/img"+p+"?w=800", nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", client.UserAgent)
	for k, v := range client.AuthHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := client.DoRaw(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 120))
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return img, nil
}

// composeCollage 把 1–3 张**互不相同**的海报按等宽分栏拼接。
//
// 只有 1 张就整幅铺满，2 张就左右各半——不再「不足 3 张时重复最后一张」：
// 那个兜底会让库里只有一张图（或多个条目共用同一张海报）时拼出三张一样的，
// 看起来就像封面坏了。
func composeCollage(imgs []image.Image) ([]byte, error) {
	if len(imgs) == 0 {
		return nil, fmt.Errorf("没有图片")
	}
	if len(imgs) > 3 {
		imgs = imgs[:3]
	}
	canvas := image.NewRGBA(image.Rect(0, 0, collageWidth, collageHeight))
	// 深色底，避免透明 PNG 出现白边
	for y := 0; y < collageHeight; y++ {
		for x := 0; x < collageWidth; x++ {
			canvas.SetRGBA(x, y, color.RGBA{R: 18, G: 18, B: 18, A: 255})
		}
	}
	cols := len(imgs)
	colW := collageWidth / cols
	for i := 0; i < cols; i++ {
		rect := image.Rect(i*colW, 0, (i+1)*colW, collageHeight)
		if i == cols-1 {
			rect.Max.X = collageWidth // 整除余数归到最后一行，避免右侧留缝
		}
		drawCover(canvas, rect, imgs[i])
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawCover 按 cover 语义（居中裁剪）把 src 画进 dst 的 rect。
func drawCover(dst *image.RGBA, rect image.Rectangle, src image.Image) {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return
	}
	want := float64(rect.Dx()) / float64(rect.Dy())
	have := float64(sw) / float64(sh)
	crop := sb
	if have > want {
		nw := int(float64(sh) * want)
		if nw < 1 {
			nw = 1
		}
		x0 := sb.Min.X + (sw-nw)/2
		crop = image.Rect(x0, sb.Min.Y, x0+nw, sb.Max.Y)
	} else if have < want {
		nh := int(float64(sw) / want)
		if nh < 1 {
			nh = 1
		}
		y0 := sb.Min.Y + (sh-nh)/2
		crop = image.Rect(sb.Min.X, y0, sb.Max.X, y0+nh)
	}
	var sub image.Image = src
	if si, ok := src.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		sub = si.SubImage(crop)
	}
	xdraw.CatmullRom.Scale(dst, rect, sub, sub.Bounds(), xdraw.Src, nil)
}

// 保持 image/png 解码器注册（sys/img 也可能返回 PNG）。
var _ = png.Decode
