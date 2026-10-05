package badgearcade

// Yaz0 compression, used for every .szs file inside Badge Arcade's archives.

import (
	"encoding/binary"
	"errors"
)

// DecompressYaz0 decompresses a Yaz0 stream.
func DecompressYaz0(src []byte) ([]byte, error) {
	if len(src) < 16 || string(src[:4]) != "Yaz0" {
		return nil, errors.New("badgearcade: not Yaz0 data")
	}
	size := int(binary.BigEndian.Uint32(src[4:]))
	out := make([]byte, 0, size)
	i := 16
	for len(out) < size {
		if i >= len(src) {
			return nil, errors.New("badgearcade: truncated Yaz0 data")
		}
		code := src[i]
		i++
		for bit := 0; bit < 8 && len(out) < size; bit++ {
			if code&(0x80>>bit) != 0 {
				if i >= len(src) {
					return nil, errors.New("badgearcade: truncated Yaz0 data")
				}
				out = append(out, src[i])
				i++
				continue
			}
			if i+1 >= len(src) {
				return nil, errors.New("badgearcade: truncated Yaz0 data")
			}
			b1, b2 := int(src[i]), int(src[i+1])
			i += 2
			dist := (b1&0xf)<<8 | b2 + 1
			n := b1 >> 4
			if n == 0 {
				if i >= len(src) {
					return nil, errors.New("badgearcade: truncated Yaz0 data")
				}
				n = int(src[i]) + 0x12
				i++
			} else {
				n += 2
			}
			if dist > len(out) {
				return nil, errors.New("badgearcade: bad Yaz0 back-reference")
			}
			for k := 0; k < n; k++ {
				out = append(out, out[len(out)-dist])
			}
		}
	}
	return out, nil
}

// CompressYaz0 compresses data as Yaz0 (greedy matching with hash chains over
// the 4 KiB window). The output differs from Nintendo's encoder but
// decompresses identically.
func CompressYaz0(data []byte) []byte {
	const (
		window = 0x1000
		minLen = 3
		maxLen = 0x111
		maxTry = 64
	)
	out := make([]byte, 16, 16+len(data)+len(data)/8+16)
	copy(out, "Yaz0")
	binary.BigEndian.PutUint32(out[4:], uint32(len(data)))

	head := make(map[uint32]int)
	prev := make([]int, len(data))
	key := func(p int) uint32 { return uint32(data[p])<<16 | uint32(data[p+1])<<8 | uint32(data[p+2]) }
	insert := func(p int) {
		if p+minLen > len(data) {
			return
		}
		k := key(p)
		if h, ok := head[k]; ok {
			prev[p] = h
		} else {
			prev[p] = -1
		}
		head[k] = p
	}

	pos := 0
	for pos < len(data) {
		codePos := len(out)
		out = append(out, 0)
		var code byte
		for bit := 0; bit < 8 && pos < len(data); bit++ {
			bestLen, bestDist := 0, 0
			if pos+minLen <= len(data) {
				if cand, ok := head[key(pos)]; ok {
					for tries := 0; cand >= 0 && pos-cand <= window && tries < maxTry; tries++ {
						l := 0
						for l < maxLen && pos+l < len(data) && data[cand+l] == data[pos+l] {
							l++
						}
						if l > bestLen {
							bestLen, bestDist = l, pos-cand
							if l == maxLen {
								break
							}
						}
						cand = prev[cand]
					}
				}
			}
			if bestLen >= minLen {
				d := bestDist - 1
				if bestLen >= 0x12 {
					out = append(out, byte(d>>8), byte(d), byte(bestLen-0x12))
				} else {
					out = append(out, byte((bestLen-2)<<4|d>>8), byte(d))
				}
				for k := 0; k < bestLen; k++ {
					insert(pos + k)
				}
				pos += bestLen
			} else {
				code |= 0x80 >> bit
				out = append(out, data[pos])
				insert(pos)
				pos++
			}
		}
		out[codePos] = code
	}
	return out
}
