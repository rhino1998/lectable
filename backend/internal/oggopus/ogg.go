package oggopus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// pageWriter is a minimal single-stream Ogg muxer (RFC 3533) that packs
// packets into pages of up to maxPageGranules, never splitting a packet
// across pages (an Opus packet is at most a few KB, well inside one
// page's 255 lacing values).
type pageWriter struct {
	out    *bytes.Buffer
	serial uint32
	seq    uint32

	segments  []byte
	body      []byte
	granule   uint64 // granule position of the last packet added
	pageStart uint64 // granule position the pending page started at
}

// writeHeaderPage writes packet on a page of its own, as RFC 7845
// requires for OpusHead and OpusTags.
func (w *pageWriter) writeHeaderPage(packet []byte, bos bool) {
	w.segments = lacing(nil, len(packet))
	w.body = append(w.body[:0], packet...)
	var flags byte
	if bos {
		flags = 0x02
	}
	w.flush(flags, 0)
}

// addPacket appends an audio packet whose last sample ends at granule,
// flushing the pending page first if it can't take it, and after it when
// the page spans maxPageGranules or last is set (which marks the page EOS).
func (w *pageWriter) addPacket(packet []byte, granule uint64, last bool) {
	if len(w.segments)+len(packet)/255+1 > 255 {
		w.flush(0, w.granule)
	}
	if len(w.segments) == 0 {
		w.pageStart = w.granule
	}
	w.segments = lacing(w.segments, len(packet))
	w.body = append(w.body, packet...)
	w.granule = granule
	switch {
	case last:
		w.flush(0x04, granule)
	case granule-w.pageStart >= maxPageGranules:
		w.flush(0, granule)
	}
}

func lacing(segments []byte, n int) []byte {
	for ; n >= 255; n -= 255 {
		segments = append(segments, 255)
	}
	return append(segments, byte(n))
}

func (w *pageWriter) flush(flags byte, granule uint64) {
	if len(w.segments) == 0 {
		return
	}
	var h [27]byte
	copy(h[:], "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint64(h[6:], granule)
	binary.LittleEndian.PutUint32(h[14:], w.serial)
	binary.LittleEndian.PutUint32(h[18:], w.seq)
	h[26] = byte(len(w.segments))
	start := w.out.Len()
	w.out.Write(h[:])
	w.out.Write(w.segments)
	w.out.Write(w.body)
	page := w.out.Bytes()[start:]
	binary.LittleEndian.PutUint32(page[22:], crc(page))
	w.seq++
	w.segments = w.segments[:0]
	w.body = w.body[:0]
}

// crcTable is Ogg's CRC-32: polynomial 0x04c11db7, MSB-first, zero init,
// no final xor - computed over the page with its checksum field zeroed.
var crcTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func crc(page []byte) uint32 {
	var c uint32
	for _, b := range page {
		c = c<<8 ^ crcTable[byte(c>>24)^b]
	}
	return c
}

// Duration reads an Ogg Opus file's playable length from its page
// headers alone: the last page's granule position less the pre-skip.
func Duration(data []byte) (time.Duration, error) {
	var preSkip, granule uint64
	pages := 0
	for off := 0; off < len(data); pages++ {
		if len(data)-off < 27 || string(data[off:off+4]) != "OggS" {
			return 0, fmt.Errorf("oggopus: bad page at byte %d", off)
		}
		nseg := int(data[off+26])
		body := off + 27 + nseg
		if body > len(data) {
			return 0, fmt.Errorf("oggopus: truncated page at byte %d", off)
		}
		size := 0
		for _, s := range data[off+27 : body] {
			size += int(s)
		}
		if body+size > len(data) {
			return 0, fmt.Errorf("oggopus: truncated page at byte %d", off)
		}
		if pages == 0 {
			if size < 19 || string(data[body:body+8]) != "OpusHead" {
				return 0, fmt.Errorf("oggopus: not an Opus stream")
			}
			preSkip = uint64(binary.LittleEndian.Uint16(data[body+10:]))
		}
		if g := binary.LittleEndian.Uint64(data[off+6:]); g != math.MaxUint64 {
			granule = g
		}
		off = body + size
	}
	if granule < preSkip {
		return 0, fmt.Errorf("oggopus: no audio")
	}
	return time.Duration(granule-preSkip) * time.Second / granuleRate, nil
}
