package badgearcade

// 3DS ETC1A4 textures, as used by Badge Arcade's Miiverse gallery (Mii.Etc1_a4,
// 128x128). Layout: the image is split into 8x8 tiles in row-major order; each
// tile holds four 4x4 blocks in the order top-left, top-right, bottom-left,
// bottom-right; each block is 8 bytes of 4-bit alpha followed by 8 bytes of
// ETC1, both stored as little-endian uint64. Inside a block, pixel (x, y) is
// index x*4+y for the alpha nibbles and the ETC1 index bits. Verified against
// a real Mii face from Nintendo's archived gallery (testdata/badgearcade_mii.etc1a4).

import (
	"encoding/binary"
	"image"
	"image/color"
	"math/bits"
)

var etc1Modifiers = [8][2]int{{2, 8}, {5, 17}, {9, 29}, {13, 42}, {18, 60}, {24, 80}, {33, 106}, {47, 183}}

// etc1a4BlockOrigins returns the top-left pixel of every 4x4 block in file order.
func etc1a4BlockOrigins(w, h int) [][2]int {
	var out [][2]int
	for ty := 0; ty < h; ty += 8 {
		for tx := 0; tx < w; tx += 8 {
			for _, o := range [][2]int{{0, 0}, {4, 0}, {0, 4}, {4, 4}} {
				out = append(out, [2]int{tx + o[0], ty + o[1]})
			}
		}
	}
	return out
}

func clamp255(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// DecodeETC1A4 decodes a w x h ETC1A4 texture.
func DecodeETC1A4(data []byte, w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for bi, o := range etc1a4BlockOrigins(w, h) {
		if (bi+1)*16 > len(data) {
			break
		}
		alpha := binary.LittleEndian.Uint64(data[bi*16:])
		block := binary.LittleEndian.Uint64(data[bi*16+8:])
		// The u64 is the standard big-endian ETC1 word; the bit layout below is
		// Ohana3DS's, which reads each half byte-swapped.
		top, bottom := bits.ReverseBytes32(uint32(block>>32)), bits.ReverseBytes32(uint32(block))
		var base [2][3]int
		if top&0x2000000 != 0 { // differential
			for c, sh := range []uint{3, 11, 19} {
				v1 := int(top>>sh) & 0x1f
				d := int(top>>(sh-3)) & 7
				if d >= 4 {
					d -= 8
				}
				v2 := v1 + d
				base[0][c] = v1<<3 | v1>>2
				base[1][c] = v2<<3 | v2>>2
			}
		} else { // individual
			for c, sh := range []uint{4, 12, 20} {
				v1, v2 := int(top>>sh)&0xf, int(top>>(sh-4))&0xf
				base[0][c], base[1][c] = v1<<4|v1, v2<<4|v2
			}
		}
		tables := [2]int{int(top>>29) & 7, int(top>>26) & 7}
		flip := top&0x1000000 != 0
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				i := uint(x*4 + y)
				half := 0
				if (!flip && x >= 2) || (flip && y >= 2) {
					half = 1
				}
				ci := (bottom>>(i+16))&1<<1 | (bottom>>i)&1
				mod := etc1Modifiers[tables[half]][ci&1]
				if ci >= 2 {
					mod = -mod
				}
				a := uint8(alpha>>(i*4)) & 0xf
				img.SetNRGBA(o[0]+x, o[1]+y, color.NRGBA{
					clamp255(base[half][0] + mod), clamp255(base[half][1] + mod), clamp255(base[half][2] + mod), a<<4 | a,
				})
			}
		}
	}
	return img
}

// EncodeETC1A4 encodes a w x h image (w, h multiples of 8) as ETC1A4, using
// ETC1's individual mode with an exhaustive search over flip and tables -
// plenty for a 128x128 Mii face.
func EncodeETC1A4(img image.Image, w, h int) []byte {
	px := func(x, y int) (int, int, int, uint8) {
		c := color.NRGBAModel.Convert(img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y)).(color.NRGBA)
		return int(c.R), int(c.G), int(c.B), c.A
	}
	origins := etc1a4BlockOrigins(w, h)
	out := make([]byte, len(origins)*16)
	for bi, o := range origins {
		var rgb [4][4][3]int
		var alpha uint64
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				r, g, b, a := px(o[0]+x, o[1]+y)
				rgb[x][y] = [3]int{r, g, b}
				alpha |= uint64(a>>4) << (uint(x*4+y) * 4)
			}
		}
		bestErr := -1
		var best uint64
		for _, flip := range []bool{false, true} {
			var top, bottom uint32
			if flip {
				top |= 0x1000000
			}
			total := 0
			for half := 0; half < 2; half++ {
				var pixels [][3]int
				var idx []uint
				for x := 0; x < 4; x++ {
					for y := 0; y < 4; y++ {
						h := 0
						if (!flip && x >= 2) || (flip && y >= 2) {
							h = 1
						}
						if h == half {
							pixels = append(pixels, rgb[x][y])
							idx = append(idx, uint(x*4+y))
						}
					}
				}
				var avg [3]int
				for _, p := range pixels {
					for c := 0; c < 3; c++ {
						avg[c] += p[c]
					}
				}
				var q [3]int // 4-bit base colour
				for c := 0; c < 3; c++ {
					q[c] = (avg[c]/len(pixels)*15 + 127) / 255
				}
				halfBest, bestTable := -1, 0
				var bestCodes []uint32
				for t := 0; t < 8; t++ {
					sum := 0
					codes := make([]uint32, len(pixels))
					for pi, p := range pixels {
						pe := -1
						for ci := uint32(0); ci < 4; ci++ {
							mod := etc1Modifiers[t][ci&1]
							if ci >= 2 {
								mod = -mod
							}
							e := 0
							for c := 0; c < 3; c++ {
								d := int(clamp255(q[c]<<4|q[c]+mod)) - p[c]
								e += d * d
							}
							if pe < 0 || e < pe {
								pe, codes[pi] = e, ci
							}
						}
						sum += pe
					}
					if halfBest < 0 || sum < halfBest {
						halfBest, bestTable, bestCodes = sum, t, codes
					}
				}
				total += halfBest
				if half == 0 {
					top |= uint32(q[0])<<4 | uint32(q[1])<<12 | uint32(q[2])<<20 | uint32(bestTable)<<29
				} else {
					top |= uint32(q[0]) | uint32(q[1])<<8 | uint32(q[2])<<16 | uint32(bestTable)<<26
				}
				for pi, i := range idx {
					bottom |= (bestCodes[pi]>>1&1)<<(i+16) | (bestCodes[pi]&1)<<i
				}
			}
			if bestErr < 0 || total < bestErr {
				bestErr, best = total, uint64(bits.ReverseBytes32(top))<<32|uint64(bits.ReverseBytes32(bottom))
			}
		}
		binary.LittleEndian.PutUint64(out[bi*16:], alpha)
		binary.LittleEndian.PutUint64(out[bi*16+8:], best)
	}
	return out
}
