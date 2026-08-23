// genicon 生成 Tg-Down 桌面客户端的图标资源（可复现，改设计后重跑即可）：
//
//	go run ./scripts/genicon -out build/appicon.png -tray internal/desktop/assets/tray.png -ico internal/desktop/assets/tray.ico
//
// 图形：圆角方形底 + 白色下载箭头（竖杆、三角头、托盘横线），纯 stdlib 光栅化。
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

var bg = [3]uint8{0x22, 0x9e, 0xd9} // 品牌蓝
var fg = [3]uint8{0xff, 0xff, 0xff}

func main() {
	out := flag.String("out", "build/appicon.png", "应用图标 PNG 输出路径")
	tray := flag.String("tray", "internal/desktop/assets/tray.png", "托盘 PNG 输出路径")
	ico := flag.String("ico", "internal/desktop/assets/tray.ico", "托盘 ICO 输出路径")
	flag.Parse()

	full := render(1024)
	must(writePNG(*out, full))

	small := render(128)
	must(writePNG(*tray, small))
	must(writeICO(*ico, render(32), render(16)))
	log.Printf("图标已生成：%s / %s / %s", *out, *tray, *ico)
}

// render 以 size×size 渲染图标
func render(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	f := float64(size)
	r := 0.18 * f // 圆角半径

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if !inRoundedRect(float64(x)+0.5, float64(y)+0.5, f, r) {
				continue
			}
			c := bg
			if inGlyph(float64(x)+0.5, float64(y)+0.5, f) {
				c = fg
			}
			img.Set(x, y, rgb(c))
		}
	}
	return img
}

func inRoundedRect(x, y, side, r float64) bool {
	if x < 0 || y < 0 || x > side || y > side {
		return false
	}
	cx := clamp(x, r, side-r)
	cy := clamp(y, r, side-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

// inGlyph 描出下载箭头：竖杆 + 三角头 + 底部托盘线。坐标按 1024 设计稿归一化。
func inGlyph(x, y, side float64) bool {
	nx, ny := x/side*1024, y/side*1024
	inRect := func(x0, y0, x1, y1 float64) bool {
		return nx >= x0 && nx <= x1 && ny >= y0 && ny <= y1
	}
	inTri := func(ax, ay, bx, by, cx, cy float64) bool {
		return sign(nx, ny, ax, ay, bx, by) <= 0 &&
			sign(nx, ny, bx, by, cx, cy) <= 0 &&
			sign(nx, ny, cx, cy, ax, ay) <= 0
	}
	switch {
	case inRect(456, 200, 568, 520): // 竖杆
		return true
	case inTri(320, 500, 704, 500, 512, 752): // 箭头
		return true
	case inRect(296, 806, 728, 838): // 托盘线
		return true
	default:
		return false
	}
}

func sign(px, py, ax, ay, bx, by float64) float64 {
	return (px-bx)*(ay-by) - (ax-bx)*(py-by)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func rgb(c [3]uint8) color.RGBA { return color.RGBA{R: c[0], G: c[1], B: c[2], A: 255} }

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func writePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeICO 写多尺寸 ICO：目录项直接内嵌 PNG 数据（Vista+ 支持）。
func writeICO(path string, imgs ...*image.RGBA) error {
	blobs := make([][]byte, len(imgs))
	sizes := make([][2]int, len(imgs))
	for i, img := range imgs {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return err
		}
		blobs[i] = buf.Bytes()
		b := img.Bounds()
		sizes[i] = [2]int{b.Dx(), b.Dy()}
	}

	var out bytes.Buffer
	put16 := func(v uint16) { _ = binary.Write(&out, binary.LittleEndian, v) }
	put32 := func(v uint32) { _ = binary.Write(&out, binary.LittleEndian, v) }

	put16(0)                  // reserved
	put16(1)                  // type: icon
	put16(uint16(len(blobs))) // count
	offset := uint32(6 + 16*len(blobs))
	for i, blob := range blobs {
		wd, ht := sizes[i][0], sizes[i][1]
		if wd >= 256 {
			wd = 0 // ICO 规范：256 记 0
		}
		if ht >= 256 {
			ht = 0
		}
		out.WriteByte(byte(wd))
		out.WriteByte(byte(ht))
		out.WriteByte(0) // 色板数（PNG 载荷为 0）
		out.WriteByte(0) // 保留
		put16(32)        // 色深
		put32(uint32(len(blob)))
		put32(offset)
		offset += uint32(len(blob))
	}
	for _, blob := range blobs {
		out.Write(blob)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}
