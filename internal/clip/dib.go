package clip

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
	"image/png"
)

// pngDIBV5 将 PNG 转为带 sRGB 色彩空间与透明通道的自底向上 Windows 位图
func pngDIBV5(data []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	// headerSize 是 Windows BITMAPV5HEADER 的字节数
	const headerSize = 124
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	stride := 4 * w
	dib := make([]byte, headerSize+stride*h)
	le := binary.LittleEndian
	le.PutUint32(dib[0:], headerSize)
	le.PutUint32(dib[4:], uint32(w))
	le.PutUint32(dib[8:], uint32(h))
	le.PutUint16(dib[12:], 1)
	le.PutUint16(dib[14:], 32)
	le.PutUint32(dib[16:], 3) // BI_BITFIELDS 指定后续通道掩码
	le.PutUint32(dib[20:], uint32(stride*h))
	le.PutUint32(dib[40:], 0x00ff0000)
	le.PutUint32(dib[44:], 0x0000ff00)
	le.PutUint32(dib[48:], 0x000000ff)
	le.PutUint32(dib[52:], 0xff000000)
	le.PutUint32(dib[56:], 0x73524742) // LCS_sRGB 指定标准 RGB 色彩空间
	le.PutUint32(dib[108:], 4)         // LCS_GM_IMAGES 指定图像渲染意图

	row := image.NRGBA{Stride: stride, Rect: image.Rect(0, 0, w, 1)}
	for y := range h {
		start := headerSize + (h-1-y)*stride
		row.Pix = dib[start : start+stride]
		draw.Draw(&row, row.Rect, src, bounds.Min.Add(image.Pt(0, y)), draw.Src)
		for i := 0; i < stride; i += 4 {
			row.Pix[i], row.Pix[i+2] = row.Pix[i+2], row.Pix[i]
		}
	}
	return dib, nil
}
