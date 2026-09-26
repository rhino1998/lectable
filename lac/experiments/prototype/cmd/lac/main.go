package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lac"
)

func readWav(path string) ([]int16, int, int) {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
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
			d := b[p+8 : p+8+n]
			pcm := make([]int16, len(d)/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(d[2*i:]))
			}
			return pcm, ch, rate
		}
		p += 8 + n + n&1
	}
	panic("no data")
}

func writeWav(path string, pcm []int16, ch, rate int) {
	b := make([]byte, 44+2*len(pcm))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], uint16(ch))
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*ch*2))
	binary.LittleEndian.PutUint16(b[32:], uint16(ch*2))
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*len(pcm)))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(v))
	}
	os.WriteFile(path, b, 0o644)
}

// parse "pe,n256/0.002,s16/11|..." into candidate configs.
func parse(spec string) []lac.Config {
	var out []lac.Config
	for _, alt := range strings.Split(spec, "|") {
		var c lac.Config
		for _, t := range strings.Split(alt, ",") {
			switch {
			case t == "pe":
				c.PreEmph = true
			case t[0] == 'm':
				c.MixMu, _ = strconv.ParseFloat(t[1:], 64)
			case t == "":
			case t[0] == 'n' || t[0] == 's':
				parts := strings.Split(t[1:], "/")
				o, _ := strconv.Atoi(parts[0])
				v, _ := strconv.ParseFloat(parts[1], 64)
				if t[0] == 'n' {
					c.Stages = append(c.Stages, lac.Stage{Kind: lac.KindNLMS, Order: o, Mu: v})
				} else {
					c.Stages = append(c.Stages, lac.Stage{Kind: lac.KindSSLMS, Order: o, Shift: uint(v)})
				}
			default:
				panic("bad token " + t)
			}
		}
		out = append(out, c)
	}
	return out
}

func encodeBest(pcm []int16, ch, rate int, cfgs []lac.Config) ([]byte, int) {
	var best []byte
	bi := 0
	for i, c := range cfgs {
		b := lac.EncodeWith(pcm, ch, rate, c)
		if best == nil || len(b) < len(best) {
			best, bi = b, i
		}
	}
	return best, bi
}

func main() {
	switch os.Args[1] {
	case "enc":
		fs := flag.NewFlagSet("enc", flag.ExitOnError)
		cfg := fs.String("cfg", "", "")
		fs.Parse(os.Args[2:])
		pcm, ch, rate := readWav(fs.Arg(0))
		b, _ := encodeBest(pcm, ch, rate, parse(*cfg))
		os.WriteFile(fs.Arg(1), b, 0o644)
	case "dec":
		b, _ := os.ReadFile(os.Args[2])
		pcm, ch, rate, err := lac.Decode(b)
		if err != nil {
			panic(err)
		}
		writeWav(os.Args[3], pcm, ch, rate)
	case "eval":
		fs := flag.NewFlagSet("eval", flag.ExitOnError)
		cfg := fs.String("cfg", "", "")
		n := fs.Int("n", 1000, "max files")
		j := fs.Int("j", 16, "")
		verify := fs.Bool("verify", true, "")
		fs.Parse(os.Args[2:])
		cfgs := parse(*cfg)
		for _, dir := range fs.Args() {
			files, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
			sort.Strings(files)
			if len(files) > *n {
				// spread the subset over the stratified list
				var sub []string
				for i := 0; i < *n; i++ {
					sub = append(sub, files[i*len(files)/(*n)])
				}
				files = sub
			}
			var raw, comp, encNs, decNs, secs atomic.Int64
			var bad atomic.Int32
			picks := make([]atomic.Int32, len(cfgs))
			var wg sync.WaitGroup
			sem := make(chan struct{}, *j)
			for _, f := range files {
				wg.Add(1)
				sem <- struct{}{}
				go func(f string) {
					defer wg.Done()
					defer func() { <-sem }()
					pcm, ch, rate := readWav(f)
					t0 := time.Now()
					b, bi := encodeBest(pcm, ch, rate, cfgs)
					encNs.Add(int64(time.Since(t0)))
					picks[bi].Add(1)
					raw.Add(int64(2 * len(pcm)))
					comp.Add(int64(len(b)))
					secs.Add(int64(1e6 * float64(len(pcm)/ch) / float64(rate)))
					if *verify {
						t1 := time.Now()
						got, _, _, err := lac.Decode(b)
						decNs.Add(int64(time.Since(t1)))
						ok := err == nil && len(got) == len(pcm)
						for i := 0; ok && i < len(got); i++ {
							ok = got[i] == pcm[i]
						}
						if !ok {
							bad.Add(1)
						}
					}
				}(f)
			}
			wg.Wait()
			s := float64(secs.Load()) / 1e6
			var pk []string
			for i := range picks {
				pk = append(pk, strconv.Itoa(int(picks[i].Load())))
			}
			fmt.Printf("%-10s %6.2f%%  enc %5.0fx dec %5.0fx  bad=%d picks=%s\n", filepath.Base(dir),
				100*float64(comp.Load())/float64(raw.Load()), s/(float64(encNs.Load())/1e9), s/(float64(decNs.Load())/1e9), bad.Load(), strings.Join(pk, "/"))
		}
	}
}

// primeGain: size(primer+x) - size(primer) vs size(x), for pairs "primer:x".
func init() {
	if len(os.Args) > 1 && os.Args[1] == "prime" {
		cfgs := parse(os.Args[2])
		var alone, primed int64
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		for _, pair := range os.Args[3:] {
			wg.Add(1)
			sem <- struct{}{}
			go func(pair string) {
				defer wg.Done()
				defer func() { <-sem }()
				pf := strings.SplitN(pair, ":", 2)
				pp, ch, rate := readWav(pf[0])
				x, _, _ := readWav(pf[1])
				a, _ := encodeBest(x, ch, rate, cfgs)
				bp, _ := encodeBest(pp, ch, rate, cfgs)
				bb, _ := encodeBest(append(append([]int16{}, pp...), x...), ch, rate, cfgs)
				mu.Lock()
				alone += int64(len(a))
				primed += int64(len(bb) - len(bp))
				mu.Unlock()
			}(pair)
		}
		wg.Wait()
		fmt.Printf("alone=%d primed=%d gain=%.2f%%\n", alone, primed, 100*(1-float64(primed)/float64(alone)))
		os.Exit(0)
	}
}
