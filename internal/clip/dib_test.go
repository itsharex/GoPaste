package clip

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestPNGDIBV5 验证 Windows 位图的通道掩码、方向、透明度和原始颜色
func TestPNGDIBV5(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 13, G: 80, B: 250, A: 37})
	img.SetNRGBA(0, 1, color.NRGBA{R: 200, G: 34, B: 1, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	dib, err := pngDIBV5(encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if len(dib) != 124+8 || le.Uint32(dib) != 124 || le.Uint32(dib[4:]) != 1 || le.Uint32(dib[8:]) != 2 ||
		le.Uint16(dib[12:]) != 1 || le.Uint16(dib[14:]) != 32 || le.Uint32(dib[16:]) != 3 || le.Uint32(dib[20:]) != 8 {
		t.Fatal("位图头与实际像素尺寸不符")
	}
	if le.Uint32(dib[40:]) != 0xff0000 || le.Uint32(dib[44:]) != 0xff00 || le.Uint32(dib[48:]) != 0xff ||
		le.Uint32(dib[52:]) != 0xff000000 || le.Uint32(dib[56:]) != 0x73524742 {
		t.Fatal("颜色通道或 sRGB 标记不正确")
	}
	if !bytes.Equal(dib[124:], []byte{1, 34, 200, 255, 250, 80, 13, 37}) {
		t.Fatalf("BGRA 像素或扫描行方向错误：%x", dib[124:])
	}
	if _, err := pngDIBV5(encoded.Bytes()[:20]); err == nil {
		t.Fatal("截断的 PNG 仍然生成了位图")
	}
}
