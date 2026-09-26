// Command harness runs lossless codecs (external commands) over the
// sample set and records size, single-core CPU time, peak RSS and a
// bit-exact PCM round-trip check for every file.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type codec struct {
	Name string
	Ext  string
	// Enc/Dec are shell commands with {in} and {out} placeholders. Dec
	// must write a WAV (or raw s16le if RawOut).
	Enc, Dec string
	RawOut   bool
}

var T = os.Getenv("TOOLS")

var codecs = []codec{
	{"flac8", "flac", "ffmpeg -v error -threads 1 -i {in} -c:a flac -compression_level 8 {out}", "ffmpeg -v error -threads 1 -i {in} -f s16le {out}", true},
	{"flac12", "flac", "ffmpeg -v error -threads 1 -i {in} -c:a flac -compression_level 12 {out}", "ffmpeg -v error -threads 1 -i {in} -f s16le {out}", true},
	{"alac", "m4a", "ffmpeg -v error -threads 1 -i {in} -c:a alac {out}", "ffmpeg -v error -threads 1 -i {in} -f s16le {out}", true},
	{"tta", "tta", "ffmpeg -v error -threads 1 -i {in} -c:a tta {out}", "ffmpeg -v error -threads 1 -i {in} -f s16le {out}", true},
	{"wavpack", "wv", "ffmpeg -v error -threads 1 -i {in} -c:a wavpack -compression_level 8 {out}", "ffmpeg -v error -threads 1 -i {in} -f s16le {out}", true},
	{"mac2000", "ape", T + "/mac/build/mac {in} {out} -c2000 -threads=1", T + "/mac/build/mac {in} {out} -d -threads=1", false},
	{"mac4000", "ape", T + "/mac/build/mac {in} {out} -c4000 -threads=1", T + "/mac/build/mac {in} {out} -d -threads=1", false},
	{"mac5000", "ape", T + "/mac/build/mac {in} {out} -c5000 -threads=1", T + "/mac/build/mac {in} {out} -d -threads=1", false},
	{"ofr2", "ofr", T + "/ofr/OptimFROG_Linux_x64_5100/ofr --encode --preset 2 --silent {in} --output {out}", T + "/ofr/OptimFROG_Linux_x64_5100/ofr --decode --silent {in} --output {out}", false},
	{"ofr5", "ofr", T + "/ofr/OptimFROG_Linux_x64_5100/ofr --encode --preset 5 --silent {in} --output {out}", T + "/ofr/OptimFROG_Linux_x64_5100/ofr --decode --silent {in} --output {out}", false},
	{"ofrmax", "ofr", T + "/ofr/OptimFROG_Linux_x64_5100/ofr --encode --preset max --silent {in} --output {out}", T + "/ofr/OptimFROG_Linux_x64_5100/ofr --decode --silent {in} --output {out}", false},
	{"xz9e", "xz", "xz -9e -T1 -c {in} > {out}", "xz -d -c {in} > {out}", false},
	{"zstd19", "zst", "zstd -q -19 --single-thread -c {in} > {out}", "zstd -q -d -c {in} > {out}", false},
	{"bzip2", "bz2", "bzip2 -9 -c {in} > {out}", "bzip2 -d -c {in} > {out}", false},
}

type result struct {
	Codec, Kind, File      string
	PCMBytes, Size         int64
	Seconds                float64 // audio duration
	EncCPU, DecCPU         float64
	EncRSSKB, DecRSSKB     int64
	OK                     bool
	Err                    string
}

func pcm(b []byte) ([]byte, int, int, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" {
		return nil, 0, 0, fmt.Errorf("not RIFF")
	}
	var ch, rate int
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		n := int(binary.LittleEndian.Uint32(b[p+4:]))
		if id == "fmt " {
			ch = int(binary.LittleEndian.Uint16(b[p+10:]))
			rate = int(binary.LittleEndian.Uint32(b[p+12:]))
		}
		if id == "data" {
			end := p + 8 + n
			if end > len(b) || n == 0 || n == 0xffffffff {
				end = len(b)
			}
			return b[p+8 : end], ch, rate, nil
		}
		p += 8 + n + n&1
	}
	return nil, 0, 0, fmt.Errorf("no data chunk")
}

func run(cmdline string) (float64, int64, error) {
	c := exec.Command("bash", "-c", cmdline)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return 0, 0, fmt.Errorf("%v: %s", err, stderr.String())
	}
	ru := c.ProcessState.SysUsage().(*syscall.Rusage)
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds()
	return cpu, ru.Maxrss, nil
}

func one(c codec, kind, in, tmp string) result {
	r := result{Codec: c.Name, Kind: kind, File: filepath.Base(in)}
	orig, _ := os.ReadFile(in)
	data, ch, rate, err := pcm(orig)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.PCMBytes = int64(len(data))
	r.Seconds = float64(len(data)) / float64(2*ch*rate)
	base := filepath.Join(tmp, c.Name+"_"+kind+"_"+strings.TrimSuffix(r.File, ".wav"))
	enc := base + "." + c.Ext
	dec := base + ".dec.wav"
	if c.RawOut {
		dec = base + ".dec.raw"
	}
	defer os.Remove(enc)
	defer os.Remove(dec)
	sub := func(s, a, b string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "{in}", a), "{out}", b)
	}
	if r.EncCPU, r.EncRSSKB, err = run(sub(c.Enc, in, enc)); err != nil {
		r.Err = "enc: " + err.Error()
		return r
	}
	st, err := os.Stat(enc)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.Size = st.Size()
	if r.DecCPU, r.DecRSSKB, err = run(sub(c.Dec, enc, dec)); err != nil {
		r.Err = "dec: " + err.Error()
		return r
	}
	got, _ := os.ReadFile(dec)
	if !c.RawOut {
		if bytes.Equal(got, orig) {
			r.OK = true
			return r
		}
		if got, _, _, err = pcm(got); err != nil {
			r.Err = "decoded: " + err.Error()
			return r
		}
	}
	r.OK = bytes.Equal(got, data)
	if !r.OK {
		r.Err = fmt.Sprintf("mismatch: %d vs %d bytes", len(got), len(data))
	}
	return r
}

func main() {
	samples := flag.String("samples", "", "samples dir (kind subdirs)")
	only := flag.String("codecs", "", "comma list (default all)")
	kinds := flag.String("kinds", "voice,variant,seed,ambience", "")
	jobs := flag.Int("j", 12, "parallel files")
	out := flag.String("out", "results.jsonl", "")
	extra := flag.String("extra", "", "extra codecs: name=encCmd|decCmd;... (dec writes WAV)")
	flag.Parse()
	cs := codecs
	for _, e := range strings.Split(*extra, ";") {
		if e == "" {
			continue
		}
		nv := strings.SplitN(e, "=", 2)
		ed := strings.SplitN(nv[1], "|", 2)
		cs = append(cs, codec{Name: nv[0], Ext: "bin", Enc: ed[0], Dec: ed[1]})
	}
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[n] = true
		}
		var f []codec
		for _, c := range cs {
			if want[c.Name] {
				f = append(f, c)
			}
		}
		cs = f
	}
	tmp, _ := os.MkdirTemp("", "hb")
	defer os.RemoveAll(tmp)
	type job struct {
		c          codec
		kind, file string
	}
	jc := make(chan job)
	var mu sync.Mutex
	var res []result
	f, _ := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	enc := json.NewEncoder(f)
	var wg sync.WaitGroup
	for i := 0; i < *jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jc {
				r := one(j.c, j.kind, j.file, tmp)
				mu.Lock()
				res = append(res, r)
				enc.Encode(r)
				if !r.OK {
					fmt.Fprintln(os.Stderr, "FAIL", r.Codec, r.Kind, r.File, r.Err)
				}
				mu.Unlock()
			}
		}()
	}
	for _, c := range cs {
		for _, k := range strings.Split(*kinds, ",") {
			files, _ := filepath.Glob(filepath.Join(*samples, k, "*.wav"))
			sort.Strings(files)
			for _, fl := range files {
				jc <- job{c, k, fl}
			}
		}
	}
	close(jc)
	wg.Wait()
	f.Close()
}
