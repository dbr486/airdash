package logos

import (
	"bytes"
	"image"
	"image/color"
	imgpng "image/png"
	"testing"
)

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := imgpng.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// bar draws an opaque red w×h bar centred on a transparent 96×96 canvas,
// the shape of the CDN's padded wordmarks.
func bar(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 96, 96))
	x0, y0 := (96-w)/2, (96-h)/2
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			img.Set(x, y, color.NRGBA{255, 0, 0, 255})
		}
	}
	return encode(t, img)
}

func red(c color.RGBA) bool   { return c.R > 200 && c.G < 60 && c.B < 60 }
func black(c color.RGBA) bool { return c.R < 30 && c.G < 30 && c.B < 30 }

func TestFit_CropsPaddingAndFitsHeightForA3to1Bar(t *testing.T) {
	img, err := Fit(bar(t, 60, 20), 104, 32)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 104 || img.Bounds().Dy() != 32 {
		t.Fatalf("bounds %v", img.Bounds())
	}
	// 3:1 bar: fitting the 32 px height gives 96 px width, centred with 4 px
	// black margins on each side; the padding of the source is gone.
	if !red(img.RGBAAt(52, 16)) || !red(img.RGBAAt(6, 16)) || !red(img.RGBAAt(97, 16)) {
		t.Errorf("bar not filling the box: %v %v %v", img.RGBAAt(52, 16), img.RGBAAt(6, 16), img.RGBAAt(97, 16))
	}
	if !black(img.RGBAAt(1, 16)) || !black(img.RGBAAt(102, 16)) {
		t.Errorf("side margins must be black: %v %v", img.RGBAAt(1, 16), img.RGBAAt(102, 16))
	}
	if !red(img.RGBAAt(52, 1)) || !red(img.RGBAAt(52, 30)) {
		t.Errorf("bar must reach the top and bottom rows: %v %v", img.RGBAAt(52, 1), img.RGBAAt(52, 30))
	}
}

func TestFit_WideBarFillsTheWidth(t *testing.T) {
	img, err := Fit(bar(t, 90, 10), 104, 32)
	if err != nil {
		t.Fatal(err)
	}
	// 9:1 bar: fitting the 104 px width gives ~12 px height, centred vertically.
	if !red(img.RGBAAt(1, 16)) || !red(img.RGBAAt(102, 16)) {
		t.Errorf("bar must span the full width: %v %v", img.RGBAAt(1, 16), img.RGBAAt(102, 16))
	}
	if !black(img.RGBAAt(52, 3)) || !black(img.RGBAAt(52, 28)) {
		t.Errorf("top and bottom must be black margins: %v %v", img.RGBAAt(52, 3), img.RGBAAt(52, 28))
	}
}

func TestFit_TransparentImageIsBlack(t *testing.T) {
	img, err := Fit(encode(t, image.NewNRGBA(image.Rect(0, 0, 96, 96))), 104, 32)
	if err != nil || !black(img.RGBAAt(52, 16)) {
		t.Errorf("err=%v px=%v", err, img.RGBAAt(52, 16))
	}
}

func TestFit_InvalidPNG(t *testing.T) {
	if _, err := Fit([]byte("nope"), 104, 32); err == nil {
		t.Error("expected error")
	}
}

func TestEncodeRGB565AndPNG(t *testing.T) {
	img, _ := Fit(bar(t, 90, 10), 104, 32)
	bm := EncodeRGB565(img)
	if len(bm) != 104*32*2 {
		t.Fatalf("len=%d", len(bm))
	}
	if v := uint16(bm[(16*104+52)*2])<<8 | uint16(bm[(16*104+52)*2+1]); v != 0xF800 {
		t.Errorf("centre pixel %04x, want red", v)
	}
	p, err := EncodePNG(img)
	if err != nil {
		t.Fatal(err)
	}
	back, err := imgpng.Decode(bytes.NewReader(p))
	if err != nil || back.Bounds().Dx() != 104 || back.Bounds().Dy() != 32 {
		t.Errorf("png round trip: err=%v bounds=%v", err, back.Bounds())
	}
}
