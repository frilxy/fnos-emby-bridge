// Command iconalpha 把图标四周的纯白背景抠成透明，保留被图形包围的白色部分
// （比如图标中间的白色桥），输出带 alpha 的 PNG。
//
// 做法是从四条边出发做连通填充：只有与边界相连、且接近白色的像素才会变透明，
// 所以图形内部的白色不会被误伤。最后对 alpha 做一次 3x3 平滑，避免锯齿。
//
// 用法：
//
//	go run ./tools/iconalpha -in fpk/图标.jpg -out fpk/图标.png
//	go run ./tools/iconalpha -in fpk/图标.jpg -out /tmp/preview.png -bg '#202020'
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
)

func main() {
	in := flag.String("in", "", "输入图片（jpg/png）")
	out := flag.String("out", "", "输出 PNG（带 alpha）")
	thr := flag.Int("threshold", 238, "视为白底的阈值（0-255，越大只抠更白的部分）")
	bg := flag.String("bg", "", "可选：把结果合成到某个底色上预览，如 #202020")
	defringe := flag.Int("defringe", 2, "去白边轮数：把紧贴透明区、仍偏白的像素也去掉，消除 JPEG 白晕")
	smooth := flag.Bool("smooth", false, "对 alpha 做 3x3 平滑（默认关，硬边更干净）")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "需要 -in 和 -out")
		os.Exit(2)
	}

	img, err := decode(*in)
	if err != nil {
		fail(err)
	}
	rgba := toRGBA(img)
	w, h := rgba.Bounds().Dx(), rgba.Bounds().Dy()

	// 1. 从四条边做连通填充，标记「外部白底」
	outer := make([]bool, w*h)
	queue := make([]int, 0, w*h)
	push := func(x, y int) {
		if x < 0 || y < 0 || x >= w || y >= h {
			return
		}
		i := y*w + x
		if outer[i] {
			return
		}
		r, g, b, _ := rgba.At(x, y).RGBA()
		if r>>8 < uint32(*thr) || g>>8 < uint32(*thr) || b>>8 < uint32(*thr) {
			return
		}
		outer[i] = true
		queue = append(queue, i)
	}
	for x := 0; x < w; x++ {
		push(x, 0)
		push(x, h-1)
	}
	for y := 0; y < h; y++ {
		push(0, y)
		push(w-1, y)
	}
	for len(queue) > 0 {
		i := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		x, y := i%w, i/w
		push(x-1, y)
		push(x+1, y)
		push(x, y-1)
		push(x, y+1)
	}

	// 2. 外部白底 → alpha 0；其余保持不透明
	alpha := make([]uint8, w*h)
	for i := range alpha {
		if outer[i] {
			alpha[i] = 0
		} else {
			alpha[i] = 255
		}
	}

	// 3. 去白边：贴着透明区、且明显偏白的像素也一齐去掉。
	// JPEG 会把白底和图形边缘混成浅灰，不去掉就会在深色背景上留一圈白晕。
	for pass := 0; pass < *defringe; pass++ {
		var next []int
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				i := y*w + x
				if alpha[i] == 0 {
					continue
				}
				if !touchesTransparent(alpha, w, h, x, y) {
					continue
				}
				r, g, b, _ := rgba.At(x, y).RGBA()
				if r>>8 >= 200 && g>>8 >= 200 && b>>8 >= 200 {
					next = append(next, i)
				}
			}
		}
		for _, i := range next {
			alpha[i] = 0
		}
	}

	// 4. 可选 alpha 平滑（默认不做，硬边在深色底上更干净）
	smoothed := make([]uint8, len(alpha))
	copy(smoothed, alpha)
	if *smooth {
		for y := 1; y < h-1; y++ {
			for x := 1; x < w-1; x++ {
				i := y*w + x
				if alpha[i] == 0 {
					continue
				}
				sum := 0
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						sum += int(alpha[(y+dy)*w+(x+dx)])
					}
				}
				smoothed[i] = uint8(sum / 9)
			}
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := rgba.At(x, y).RGBA()
			dst.SetNRGBA(x, y, color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: smoothed[y*w+x]})
		}
	}

	if *bg != "" {
		c, err := parseHex(*bg)
		if err != nil {
			fail(err)
		}
		canvas := image.NewNRGBA(dst.Bounds())
		draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
		draw.Draw(canvas, canvas.Bounds(), dst, image.Point{}, draw.Over)
		dst = canvas
	}

	f, err := os.Create(*out)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	if err := png.Encode(f, dst); err != nil {
		fail(err)
	}
	cleared := 0
	for _, a := range smoothed {
		if a == 0 {
			cleared++
		}
	}
	fmt.Printf("%s → %s（%dx%d，透明像素 %d/%d）\n", *in, *out, w, h, cleared, w*h)
}

func touchesTransparent(alpha []uint8, w, h, x, y int) bool {
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			nx, ny := x+dx, y+dy
			if nx < 0 || ny < 0 || nx >= w || ny >= h {
				continue
			}
			if alpha[ny*w+nx] == 0 {
				return true
			}
		}
	}
	return false
}

func hasOpaqueNeighbor(alpha []uint8, w, h, x, y int) bool {
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			nx, ny := x+dx, y+dy
			if nx < 0 || ny < 0 || nx >= w || ny >= h {
				continue
			}
			if alpha[ny*w+nx] != 0 {
				return true
			}
		}
	}
	return false
}

func decode(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if isPNG(path) {
		return png.Decode(f)
	}
	return jpeg.Decode(f)
}

func isPNG(path string) bool {
	return len(path) > 4 && (path[len(path)-4:] == ".png" || path[len(path)-4:] == ".PNG")
}

func toRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok {
		return r
	}
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, img, b.Min, draw.Src)
	return dst
}

func parseHex(s string) (color.NRGBA, error) {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		return color.NRGBA{}, fmt.Errorf("颜色格式应为 #RRGGBB: %w", err)
	}
	return color.NRGBA{R: r, G: g, B: b, A: 255}, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "iconalpha:", err)
	os.Exit(1)
}
