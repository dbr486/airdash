package logos

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// The CDN pads every wordmark into a square, so most of a square logo is
// empty. Fit crops the PNG to its opaque bounding box, scales that to fit
// inside w×h (aspect preserved, area-averaged) and centres it on black,
// the panel background. A fully transparent image gives a black box.
func Fit(data []byte, w, h int) (*image.RGBA, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("logo fit: bad size %dx%d", w, h)
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("logo png: %w", err)
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.Black, image.Point{}, draw.Src)

	box, ok := opaqueBounds(src)
	if !ok {
		return out, nil
	}
	flat := image.NewRGBA(box)
	draw.Draw(flat, box, image.Black, image.Point{}, draw.Src)
	draw.Draw(flat, box, src, box.Min, draw.Over)

	bw, bh := box.Dx(), box.Dy()
	dw, dh := w, w*bh/bw // fit the width first
	if dh > h {
		dh, dw = h, h*bw/bh // too tall: fit the height instead
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	ox, oy := (w-dw)/2, (h-dh)/2
	for y := 0; y < dh; y++ {
		sy0, sy1 := y*bh/dh, (y+1)*bh/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < dw; x++ {
			sx0, sx1 := x*bw/dw, (x+1)*bw/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, b, n uint32
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					c := flat.RGBAAt(box.Min.X+sx, box.Min.Y+sy)
					r += uint32(c.R)
					g += uint32(c.G)
					b += uint32(c.B)
					n++
				}
			}
			out.SetRGBA(ox+x, oy+y, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), 255})
		}
	}
	return out, nil
}

// opaqueBounds is the tight rectangle around pixels that are more than
// about 12% opaque; anti-aliased fringes below that count as padding.
func opaqueBounds(img image.Image) (image.Rectangle, bool) {
	b := img.Bounds()
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a > 0x2000 {
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < minX {
		return image.Rectangle{}, false
	}
	return image.Rect(minX, minY, maxX+1, maxY+1), true
}

// ValidBitmapSize reports whether the legacy square route serves that size.
func ValidBitmapSize(n int) bool { return n == 24 || n == 32 || n == 48 }

// EncodeRGB565 packs an RGBA image as big-endian RGB565, row-major, the
// byte order the ST7789 takes over SPI.
func EncodeRGB565(img *image.RGBA) []byte {
	b := img.Bounds()
	out := make([]byte, 0, b.Dx()*b.Dy()*2)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			v := uint16(c.R>>3)<<11 | uint16(c.G>>2)<<5 | uint16(c.B>>3)
			out = append(out, byte(v>>8), byte(v))
		}
	}
	return out
}

func EncodePNG(img *image.RGBA) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
