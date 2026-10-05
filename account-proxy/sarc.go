package main

// Minimal SARC archive reader/writer (little-endian, as used on 3DS).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
)

type sarcEntry struct {
	Name string
	Data []byte
}

const sarcHashMultiplier = 0x65

func sarcHash(name string) uint32 {
	var h uint32
	for i := 0; i < len(name); i++ {
		h = h*sarcHashMultiplier + uint32(name[i])
	}
	return h
}

// parseSARC returns the archive's entries in stored (hash) order.
func parseSARC(b []byte) ([]sarcEntry, error) {
	if len(b) < 0x20 || string(b[:4]) != "SARC" || b[6] != 0xff || b[7] != 0xfe {
		return nil, errors.New("sarc: not a little-endian SARC")
	}
	hlen := int(binary.LittleEndian.Uint16(b[4:]))
	dataOff := int(binary.LittleEndian.Uint32(b[12:]))
	if string(b[hlen:hlen+4]) != "SFAT" {
		return nil, errors.New("sarc: missing SFAT")
	}
	count := int(binary.LittleEndian.Uint16(b[hlen+6:]))
	nodes := hlen + 12
	names := nodes + 16*count + 8
	if names > len(b) || string(b[names-8:names-4]) != "SFNT" {
		return nil, errors.New("sarc: missing SFNT")
	}
	entries := make([]sarcEntry, 0, count)
	for i := 0; i < count; i++ {
		n := b[nodes+16*i:]
		attr := binary.LittleEndian.Uint32(n[4:])
		start := dataOff + int(binary.LittleEndian.Uint32(n[8:]))
		end := dataOff + int(binary.LittleEndian.Uint32(n[12:]))
		if start > end || end > len(b) {
			return nil, errors.New("sarc: entry out of range")
		}
		var name string
		if attr>>24 != 0 {
			off := names + int(attr&0xffffff)*4
			if off >= len(b) {
				return nil, errors.New("sarc: name out of range")
			}
			l := bytes.IndexByte(b[off:], 0)
			if l < 0 {
				return nil, errors.New("sarc: unterminated name")
			}
			name = string(b[off : off+l])
		}
		entries = append(entries, sarcEntry{Name: name, Data: b[start:end]})
	}
	return entries, nil
}

// buildSARC writes entries sorted by name hash. Each file's data is aligned to
// fileAlign within the data section, which itself starts at a multiple of
// dataAlign. Nintendo's Badge Arcade archives use (4, 16) for data_v131.dat's
// outer archive and (128, 128) for each post archive.
func buildSARC(entries []sarcEntry, fileAlign, dataAlign int) []byte {
	sorted := append([]sarcEntry{}, entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sarcHash(sorted[i].Name) < sarcHash(sorted[j].Name) })

	var names bytes.Buffer
	nameOffsets := make([]int, len(sorted))
	for i, e := range sorted {
		nameOffsets[i] = names.Len()
		names.WriteString(e.Name)
		names.WriteByte(0)
		for names.Len()%4 != 0 {
			names.WriteByte(0)
		}
	}
	align := func(v, a int) int { return (v + a - 1) / a * a }

	const hdrLen = 0x14
	sfat := hdrLen + 12 + 16*len(sorted)
	dataOff := align(sfat+8+names.Len(), dataAlign)

	var data bytes.Buffer
	nodes := make([]byte, 16*len(sorted))
	for i, e := range sorted {
		for data.Len()%fileAlign != 0 {
			data.WriteByte(0)
		}
		start := data.Len()
		data.Write(e.Data)
		n := nodes[16*i:]
		binary.LittleEndian.PutUint32(n[0:], sarcHash(e.Name))
		binary.LittleEndian.PutUint32(n[4:], 1<<24|uint32(nameOffsets[i]/4))
		binary.LittleEndian.PutUint32(n[8:], uint32(start))
		binary.LittleEndian.PutUint32(n[12:], uint32(data.Len()))
	}

	out := make([]byte, dataOff, dataOff+data.Len())
	copy(out, "SARC")
	binary.LittleEndian.PutUint16(out[4:], hdrLen)
	out[6], out[7] = 0xff, 0xfe
	binary.LittleEndian.PutUint32(out[12:], uint32(dataOff))
	binary.LittleEndian.PutUint16(out[16:], 0x0100)
	copy(out[hdrLen:], "SFAT")
	binary.LittleEndian.PutUint16(out[hdrLen+4:], 12)
	binary.LittleEndian.PutUint16(out[hdrLen+6:], uint16(len(sorted)))
	binary.LittleEndian.PutUint32(out[hdrLen+8:], sarcHashMultiplier)
	copy(out[hdrLen+12:], nodes)
	copy(out[sfat:], "SFNT")
	binary.LittleEndian.PutUint16(out[sfat+4:], 8)
	copy(out[sfat+8:], names.Bytes())
	out = append(out, data.Bytes()...)
	binary.LittleEndian.PutUint32(out[8:], uint32(len(out)))
	return out
}
