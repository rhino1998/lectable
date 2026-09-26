package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

func main() {
	for _, kind := range os.Args[2:] {
		files, _ := filepath.Glob(filepath.Join(os.Args[1], kind, "*.wav"))
		var raw, comp int64
		var secs, encT, decT float64
		bad := 0
		for _, f := range files {
			b, _ := os.ReadFile(f)
			ch := int(binary.LittleEndian.Uint16(b[22:]))
			rate := int(binary.LittleEndian.Uint32(b[24:]))
			d := b[44:]
			n := len(d) / 2 / ch
			t0 := time.Now()
			var out bytes.Buffer
			info := &meta.StreamInfo{BlockSizeMin: 4096, BlockSizeMax: 4096, SampleRate: uint32(rate), NChannels: uint8(ch), BitsPerSample: 16, NSamples: uint64(n)}
			enc, err := flac.NewEncoder(&out, info)
			if err != nil {
				panic(err)
			}
			for start := 0; start < n; start += 4096 {
				m := min(4096, n-start)
				fr := &frame.Frame{Header: frame.Header{HasFixedBlockSize: true, BlockSize: uint16(m), SampleRate: uint32(rate), Channels: frame.Channels(ch - 1), BitsPerSample: 16}}
				for c := 0; c < ch; c++ {
					s := make([]int32, m)
					for i := range s {
						s[i] = int32(int16(binary.LittleEndian.Uint16(d[2*((start+i)*ch+c):])))
					}
					fr.Subframes = append(fr.Subframes, &frame.Subframe{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: s, NSamples: m})
				}
				if err := enc.WriteFrame(fr); err != nil {
					panic(err)
				}
			}
			enc.Close()
			encT += time.Since(t0).Seconds()
			t1 := time.Now()
			st, err := flac.New(bytes.NewReader(out.Bytes()))
			if err != nil {
				panic(err)
			}
			i := 0
			ok := true
			for {
				fr, err := st.ParseNext()
				if err == io.EOF {
					break
				}
				if err != nil {
					panic(err)
				}
				for k := 0; k < int(fr.BlockSize); k++ {
					for c := 0; c < ch; c++ {
						if int16(fr.Subframes[c].Samples[k]) != int16(binary.LittleEndian.Uint16(d[2*((i+k)*ch+c):])) {
							ok = false
						}
					}
				}
				i += int(fr.BlockSize)
			}
			decT += time.Since(t1).Seconds()
			if !ok || i != n {
				bad++
			}
			raw += int64(len(d))
			comp += int64(out.Len())
			secs += float64(n) / float64(rate)
		}
		fmt.Printf("%-9s mewkiz/flac %.2f%%  enc %.0fx dec %.0fx bad=%d\n", kind, 100*float64(comp)/float64(raw), secs/encT, secs/decT, bad)
	}
}
