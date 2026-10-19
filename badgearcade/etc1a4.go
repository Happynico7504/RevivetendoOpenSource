package badgearcade

// 3DS ETC1 and ETC1A4 textures: badge art (.prb, 128x128 ETC1A4), the Miiverse
// gallery (Mii.Etc1_a4, 128x128 ETC1A4) and crane stages (.crb, ETC1).
// Layout: the image is split into 8x8 tiles in row-major order; each tile
// holds four 4x4 blocks in the order top-left, top-right, bottom-left,
// bottom-right. An ETC1 block is the standard ETC1 64-bit word stored as a
// little-endian uint64; ETC1A4 puts 8 bytes of 4-bit alpha (little-endian
// uint64) before each one. Inside a block, pixel (x, y) is index x*4+y for the
// alpha nibbles and the ETC1 index bits. Verified against Nintendo's badges
// and crane stages: decoding them any other way gives visibly noisy blocks.

import (
	"encoding/binary"
	"image"
	"image/color"
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

// etc1Half reports which half (sub-block) of a 4x4 block pixel (x, y) is in.
func etc1Half(x, y int, flip bool) int {
	if (!flip && x >= 2) || (flip && y >= 2) {
		return 1
	}
	return 0
}

// decodeETC1Block returns the 4x4 pixels of an ETC1 word, indexed [x][y].
func decodeETC1Block(word uint64) (px [4][4][3]uint8) {
	hi, lo := uint32(word>>32), uint32(word)
	var base [2][3]int
	if hi&2 != 0 { // differential
		for c, sh := range []uint{27, 19, 11} {
			v1 := int(hi>>sh) & 0x1f
			d := int(hi>>(sh-3)) & 7
			if d >= 4 {
				d -= 8
			}
			v2 := (v1 + d) & 0x1f
			base[0][c] = v1<<3 | v1>>2
			base[1][c] = v2<<3 | v2>>2
		}
	} else { // individual
		for c, sh := range []uint{28, 20, 12} {
			v1, v2 := int(hi>>sh)&0xf, int(hi>>(sh-4))&0xf
			base[0][c], base[1][c] = v1<<4|v1, v2<<4|v2
		}
	}
	tables := [2]int{int(hi>>5) & 7, int(hi>>2) & 7}
	flip := hi&1 != 0
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			i := uint(x*4 + y)
			half := etc1Half(x, y, flip)
			ci := (lo>>(i+16))&1<<1 | (lo>>i)&1
			mod := etc1Modifiers[tables[half]][ci&1]
			if ci >= 2 {
				mod = -mod
			}
			for c := 0; c < 3; c++ {
				px[x][y][c] = clamp255(base[half][c] + mod)
			}
		}
	}
	return px
}

// encodeETC1Block encodes 4x4 pixels (indexed [x][y]) as an ETC1 word, trying
// both flips in individual and (where the base colours are close enough)
// differential mode, with an exhaustive search over the modifier tables.
func encodeETC1Block(rgb [4][4][3]int) uint64 {
	bestErr := -1
	var best uint64
	for _, flip := range []bool{false, true} {
		var halves [2][][3]int
		var idx [2][]uint
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				h := etc1Half(x, y, flip)
				halves[h] = append(halves[h], rgb[x][y])
				idx[h] = append(idx[h], uint(x*4+y))
			}
		}
		var avg [2][3]int
		for h := 0; h < 2; h++ {
			for _, p := range halves[h] {
				for c := 0; c < 3; c++ {
					avg[h][c] += p[c]
				}
			}
			for c := 0; c < 3; c++ {
				avg[h][c] /= len(halves[h])
			}
		}
		for _, diff := range []bool{false, true} {
			var hi uint32
			var base [2][3]int
			ok := true
			for c, sh := range []uint{27, 19, 11} {
				if diff {
					v1, v2 := (avg[0][c]*31+127)/255, (avg[1][c]*31+127)/255
					d := v2 - v1
					if d < -4 || d > 3 {
						ok = false
						break
					}
					hi |= uint32(v1)<<sh | uint32(d&7)<<(sh-3)
					base[0][c], base[1][c] = v1<<3|v1>>2, v2<<3|v2>>2
				} else {
					v1, v2 := (avg[0][c]*15+127)/255, (avg[1][c]*15+127)/255
					hi |= uint32(v1)<<(sh+1) | uint32(v2)<<(sh-3)
					base[0][c], base[1][c] = v1<<4|v1, v2<<4|v2
				}
			}
			if !ok {
				continue
			}
			if diff {
				hi |= 2
			}
			if flip {
				hi |= 1
			}
			var lo uint32
			total := 0
			for h := 0; h < 2; h++ {
				halfBest, bestTable := -1, 0
				var bestCodes []uint32
				for t := 0; t < 8; t++ {
					sum := 0
					codes := make([]uint32, len(halves[h]))
					for pi, p := range halves[h] {
						pe := -1
						for ci := uint32(0); ci < 4; ci++ {
							mod := etc1Modifiers[t][ci&1]
							if ci >= 2 {
								mod = -mod
							}
							e := 0
							for c := 0; c < 3; c++ {
								d := int(clamp255(base[h][c]+mod)) - p[c]
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
				hi |= uint32(bestTable) << (5 - 3*uint(h))
				for pi, i := range idx[h] {
					lo |= (bestCodes[pi]>>1&1)<<(i+16) | (bestCodes[pi]&1)<<i
				}
			}
			if bestErr < 0 || total < bestErr {
				bestErr, best = total, uint64(hi)<<32|uint64(lo)
			}
		}
	}
	return best
}

// DecodeETC1A4 decodes a w x h ETC1A4 texture.
func DecodeETC1A4(data []byte, w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for bi, o := range etc1a4BlockOrigins(w, h) {
		if (bi+1)*16 > len(data) {
			break
		}
		alpha := binary.LittleEndian.Uint64(data[bi*16:])
		px := decodeETC1Block(binary.LittleEndian.Uint64(data[bi*16+8:]))
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				a := uint8(alpha>>(uint(x*4+y)*4)) & 0xf
				p := px[x][y]
				img.SetNRGBA(o[0]+x, o[1]+y, color.NRGBA{p[0], p[1], p[2], a<<4 | a})
			}
		}
	}
	return img
}

// DecodeETC1 decodes a w x h ETC1 texture (opaque).
func DecodeETC1(data []byte, w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for bi, o := range etc1a4BlockOrigins(w, h) {
		if (bi+1)*8 > len(data) {
			break
		}
		px := decodeETC1Block(binary.LittleEndian.Uint64(data[bi*8:]))
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				p := px[x][y]
				img.SetNRGBA(o[0]+x, o[1]+y, color.NRGBA{p[0], p[1], p[2], 255})
			}
		}
	}
	return img
}

// blockPixels reads the 4x4 block at o from img as [x][y] RGB plus the
// block's 4-bit alpha word.
func blockPixels(img image.Image, o [2]int) (rgb [4][4][3]int, alpha uint64) {
	b := img.Bounds()
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+o[0]+x, b.Min.Y+o[1]+y)).(color.NRGBA)
			rgb[x][y] = [3]int{int(c.R), int(c.G), int(c.B)}
			alpha |= uint64(c.A>>4) << (uint(x*4+y) * 4)
		}
	}
	return
}

// EncodeETC1A4 encodes a w x h image (w, h multiples of 8) as ETC1A4.
func EncodeETC1A4(img image.Image, w, h int) []byte {
	origins := etc1a4BlockOrigins(w, h)
	out := make([]byte, len(origins)*16)
	for bi, o := range origins {
		rgb, alpha := blockPixels(img, o)
		binary.LittleEndian.PutUint64(out[bi*16:], alpha)
		binary.LittleEndian.PutUint64(out[bi*16+8:], encodeETC1Block(rgb))
	}
	return out
}

// EncodeETC1 encodes a w x h image (w, h multiples of 8) as ETC1; alpha is
// ignored.
func EncodeETC1(img image.Image, w, h int) []byte {
	origins := etc1a4BlockOrigins(w, h)
	out := make([]byte, len(origins)*8)
	for bi, o := range origins {
		rgb, _ := blockPixels(img, o)
		binary.LittleEndian.PutUint64(out[bi*8:], encodeETC1Block(rgb))
	}
	return out
}
