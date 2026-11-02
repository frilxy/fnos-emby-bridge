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
	// 影视库卡片是横向的；竖版 600×900 会被客户端按横卡裁切，导致海报被放大。
	// 用 1200×750 横版画布，三张 2:3 海报完整并排。
	collageWidth  = 1200
	collageHeight = 750
	collageTTL    = 30 * time.Minute
)

func collageBucket() int64 { return time.Now().Unix() / int64(collageTTL/time.Second) }

// collageTag 给 MediaDB 的 PrimaryImageTag：同一个 30 分钟桶内稳定，过期后变化。
func collageTag(guid string) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%d", guid, collageBucket())
	return fmt.Sprintf("c%x", h.Sum64())
}

// libraryCollage 生成/缓存库封面。
func (h *Handler) libraryCollage(ctx context.Context, client *fn.Client, libID string) ([]byte, error) {
	bucket := collageBucket()
	key := fmt.Sprintf("collage:%s:%s:%d", client.Token, libID, bucket)
	v, err := h.cache.Do(key, collageTTL, func() (any, error) {
		list, err := h.typedItems(ctx, client, libID, []string{"Movie", "TV", "Video", "Directory"}, 0)
		if err != nil {
			return nil, err
		}
		picks := pickCollageItems(list, libID, bucket)
		if len(picks) == 0 {
			return nil, fmt.Errorf("库 %s 没有可用的海报", libID)
		}
		imgs := make([]image.Image, 0, len(picks))
		for _, it := range picks {
			p := it.PosterPath()
			if p == "" {
				p = it.BackdropPath()
			}
			if p == "" {
				continue
			}
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

// pickCollageItems 用库 guid + 时间桶做种子，稳定随机挑 3 个有图的条目。
func pickCollageItems(list []fn.MediaItem, libID string, bucket int64) []fn.MediaItem {
	candidates := make([]fn.MediaItem, 0, len(list))
	for _, m := range list {
		if m.PosterPath() != "" || m.BackdropPath() != "" {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%d", libID, bucket)
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

// composeCollage 把 1–3 张海报按竖版三栏拼接；不足 3 张时重复最后一张。
func composeCollage(imgs []image.Image) ([]byte, error) {
	if len(imgs) == 0 {
		return nil, fmt.Errorf("没有图片")
	}
	canvas := image.NewRGBA(image.Rect(0, 0, collageWidth, collageHeight))
	// 深色底，避免透明 PNG 出现白边
	for y := 0; y < collageHeight; y++ {
		for x := 0; x < collageWidth; x++ {
			canvas.SetRGBA(x, y, color.RGBA{R: 18, G: 18, B: 18, A: 255})
		}
	}
	const (
		cols    = 3
		posterW = 300
		posterH = 450
		gap     = 40
	)
	totalW := cols*posterW + (cols-1)*gap
	startX := (collageWidth - totalW) / 2
	startY := (collageHeight - posterH) / 2
	for i := 0; i < cols; i++ {
		img := imgs[len(imgs)-1]
		if i < len(imgs) {
			img = imgs[i]
		}
		x := startX + i*(posterW+gap)
		rect := image.Rect(x, startY, x+posterW, startY+posterH)
		drawCover(canvas, rect, img)
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
