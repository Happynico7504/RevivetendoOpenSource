package badgearcade

// RGB565 and 4-bit alpha textures in the 3DS's tiled layout: 8x8 tiles in
// row-major order, pixels inside a tile in Morton (Z) order. Badge images
// (.prb) store an RGB565 plane followed by a separate A4 plane (two pixels per
// byte, low nibble first).

import (
	"encoding/binary"
	"image"
	"image/color"
)

// tiledPixels returns (x, y) for every pixel of a w x h texture in file order.
func tiledPixels(w, h int) [][2]int {
	out := make([][2]int, 0, w*h)
	for ty := 0; ty < h; ty += 8 {
		for tx := 0; tx < w; tx += 8 {
			for i := 0; i < 64; i++ {
				x, y := 0, 0
				for b := 0; b < 3; b++ {
					x |= (i >> (2 * b) & 1) << b
					y |= (i >> (2*b + 1) & 1) << b
				}
				out = append(out, [2]int{tx + x, ty + y})
			}
		}
	}
	return out
}

// RGB565A4Size is the byte size of a w x h RGB565 plane plus its A4 plane.
func RGB565A4Size(w, h int) int { return w*h*2 + w*h/2 }

// DecodeRGB565A4 decodes an RGB565 plane followed by an A4 plane.
func DecodeRGB565A4(data []byte, w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	alpha := data[w*h*2:]
	for i, p := range tiledPixels(w, h) {
		v := binary.LittleEndian.Uint16(data[i*2:])
		a := alpha[i/2] >> (4 * uint(i&1)) & 0xf
		img.SetNRGBA(p[0], p[1], color.NRGBA{
			uint8(int(v>>11&31) * 255 / 31), uint8(int(v>>5&63) * 255 / 63), uint8(int(v&31) * 255 / 31), a<<4 | a,
		})
	}
	return img
}

// EncodeRGB565A4 encodes img (w x h, multiples of 8) as an RGB565 plane plus an
// A4 plane.
func EncodeRGB565A4(img image.Image, w, h int) []byte {
	out := make([]byte, RGB565A4Size(w, h))
	alpha := out[w*h*2:]
	b := img.Bounds()
	for i, p := range tiledPixels(w, h) {
		c := color.NRGBAModel.Convert(img.At(b.Min.X+p[0], b.Min.Y+p[1])).(color.NRGBA)
		v := uint16(c.R>>3)<<11 | uint16(c.G>>2)<<5 | uint16(c.B>>3)
		binary.LittleEndian.PutUint16(out[i*2:], v)
		alpha[i/2] |= (c.A >> 4) << (4 * uint(i&1))
	}
	return out
}
