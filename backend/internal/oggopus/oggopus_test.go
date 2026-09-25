package oggopus

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"math/rand"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/kazzmir/opus-go/ogg"
	"github.com/kazzmir/opus-go/opus"

	"github.com/rhino1998/lectable/backend/internal/wav"
)

// decoded is an encoded stream played back the way a player would:
// decoded at 48kHz, pre-skip dropped, end-trimmed to the last granule.
type decoded struct {
	samples  []float32 // interleaved, 48kHz
	channels int
	pages    int
	eos      bool
}

func decode(t *testing.T, data []byte) decoded {
	t.Helper()
	r, err := ogg.NewOpusReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("read headers: %v", err)
	}
	r.SetVerifyCRC(true)
	ch := int(r.Head.Channels)
	dec, err := opus.NewDecoder(granuleRate, ch)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var pcm []float32
	var last *ogg.OpusAudioPacket
	buf := make([]float32, 5760*ch)
	var pinner runtime.Pinner // see Encode's own pinning
	defer pinner.Unpin()
	pinner.Pin(&buf[0])
	for {
		p, err := r.ReadAudioPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read packet: %v", err)
		}
		n, err := dec.DecodeF32(p.Data, buf, 5760, false)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		pcm = append(pcm, buf[:n*ch]...)
		last = p
	}
	if last == nil {
		t.Fatal("no audio packets")
	}
	pre := int(r.Head.PreSkip)
	end := int(last.GranulePos)
	if end > len(pcm)/ch {
		t.Fatalf("final granule %d past the %d decodable samples", end, len(pcm)/ch)
	}
	return decoded{
		samples:  pcm[pre*ch : end*ch],
		channels: ch,
		pages:    bytes.Count(data, []byte("OggS")),
		eos:      last.EOS,
	}
}

func sine(rate, channels, frames int, hz float64) *wav.Clip {
	s := make([]float32, frames*channels)
	for i := range frames {
		v := float32(0.5 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		for c := range channels {
			s[i*channels+c] = v
		}
	}
	return &wav.Clip{Samples: s, SampleRate: rate, Channels: channels}
}

// crossings counts sign changes in channel 0 - 2 per cycle of a sine.
func crossings(s []float32, channels int) int {
	n := 0
	for i := channels; i < len(s); i += channels {
		if (s[i-channels] < 0) != (s[i] < 0) {
			n++
		}
	}
	return n
}

func TestRoundTrip(t *testing.T) {
	const hz = 440.0
	for _, tc := range []struct {
		rate, channels, frames int
	}{
		{24000, 1, 24000},       // native rate, whole number of 20ms frames
		{24000, 1, 24000 + 400}, // last frame padded by less than the lookahead
		{24000, 1, 24000 + 1},
		{48000, 2, 48000 * 2},
		{44100, 2, 44100 * 2}, // resampled
		{22050, 1, 22050},     // resampled
		{16000, 1, 16000 + 7},
	} {
		clip := sine(tc.rate, tc.channels, tc.frames, hz)
		data, err := Encode(clip)
		if err != nil {
			t.Fatalf("%+v: encode: %v", tc, err)
		}
		d := decode(t, data)
		want := int(math.Ceil(float64(tc.frames) * granuleRate / float64(tc.rate)))
		if got := len(d.samples) / d.channels; got != want {
			t.Errorf("%+v: decoded %d samples, want %d", tc, got, want)
		}
		if d.channels != tc.channels {
			t.Errorf("%+v: %d channels, want %d", tc, d.channels, tc.channels)
		}
		if dur, err := Duration(data); err != nil {
			t.Errorf("%+v: Duration: %v", tc, err)
		} else if got, want := dur.Seconds(), float64(want)/granuleRate; math.Abs(got-want) > 1e-6 {
			t.Errorf("%+v: Duration %.6fs, want %.6fs", tc, got, want)
		}
		if !d.eos {
			t.Errorf("%+v: last page isn't EOS", tc)
		}
		seconds := float64(tc.frames) / float64(tc.rate)
		if got, want := float64(crossings(d.samples, d.channels))/seconds/2, hz; math.Abs(got-want) > 5 {
			t.Errorf("%+v: decoded pitch %.1fHz, want %.0fHz", tc, got, want)
		}
		// The real last samples must survive, not trail off into the
		// encoder's lookahead: the final 5ms should still carry the sine.
		tail := d.samples[len(d.samples)-240*d.channels:]
		var peak float32
		for _, v := range tail {
			peak = max(peak, float32(math.Abs(float64(v))))
		}
		if peak < 0.25 {
			t.Errorf("%+v: tail peak %.3f, the clip's end was lost", tc, peak)
		}
		// About one page per second of audio plus the two header pages.
		if maxPages := int(seconds) + 4; d.pages > maxPages {
			t.Errorf("%+v: %d pages, want at most %d", tc, d.pages, maxPages)
		}
	}
}

func TestEncodeWAVCompresses(t *testing.T) {
	clip := sine(24000, 1, 24000*5, 220)
	in, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeWAV(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out)*4 > len(in) {
		t.Errorf("encoded %d bytes from %d of WAV - expected well under a quarter", len(out), len(in))
	}
}

func TestRejectsMultichannel(t *testing.T) {
	if _, err := Encode(&wav.Clip{Samples: make([]float32, 600), SampleRate: 48000, Channels: 6}); err == nil {
		t.Fatal("expected an error for 6 channels")
	}
}

// TestDeterministic guards Encode's buffer pinning: with them on the
// stack, a GC-driven stack shrink between calls let opus-go's encoder
// write a packet into the stack's stale copy, so identical input gave
// differing (zero-packet) output.
func TestDeterministic(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	clip := sine(24000, 1, 24000, 440)
	var want [32]byte
	for i := range 8 {
		data, err := Encode(clip)
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(data)
		if i == 0 {
			want = got
		} else if got != want {
			t.Fatalf("encode %d differs from the first", i)
		}
		runtime.GC()
	}
}

// speechLike is a crude speech stand-in that libopus reliably hands to
// SILK/hybrid when allowed (about 80% of packets at 48 kbps): a gliding
// 120 Hz pulse train plus breath noise through two moving formants, in
// 250 ms syllables with every third one silent, at a quiet level.
func speechLike(rate, frames int) *wav.Clip {
	rng := rand.New(rand.NewSource(1))
	s := make([]float32, frames)
	var y1, y2, z1, z2, phase float64
	for i := range frames {
		sec := float64(i) / float64(rate)
		phase += (120 + 30*math.Sin(2*math.Pi*1.3*sec)) / float64(rate)
		var x float64
		if phase >= 1 {
			phase--
			x = 1
		}
		if int(sec*4)%3 == 2 {
			x = 0
		}
		x += 0.1 * rng.NormFloat64()
		f1 := 500 + 300*math.Sin(2*math.Pi*2.1*sec)
		f2 := 1500 + 600*math.Sin(2*math.Pi*1.7*sec)
		c1 := 2 * 0.97 * math.Cos(2*math.Pi*f1/float64(rate))
		c2 := 2 * 0.95 * math.Cos(2*math.Pi*f2/float64(rate))
		y := x + c1*y1 - 0.97*0.97*y2
		y2, y1 = y1, y
		z := y + c2*z1 - 0.95*0.95*z2
		z2, z1 = z1, z
		s[i] = float32(z * 0.005)
	}
	return &wav.Clip{Samples: s, SampleRate: rate, Channels: 1}
}

// TestSpeechSurvivesSILK encodes speech-like input that libopus codes
// with SILK/hybrid and checks it decodes back close to the input. With
// opus-go v1.4.0's libc shim (zero-extending (uint32)(int8) casts) every
// SILK frame came out as garbage - an SNR near -19 dB on real narration.
func TestSpeechSurvivesSILK(t *testing.T) {
	clip := speechLike(24000, 24000*3)
	data, err := Encode(clip)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ogg.NewOpusReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	silk := 0
	for {
		p, err := r.ReadAudioPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.Data[0]>>3 < 16 {
			silk++
		}
	}
	if silk == 0 {
		t.Fatal("no SILK/hybrid packets - the test input no longer exercises SILK")
	}

	// Decoded speech must track the input, not bury it in noise.
	d := decode(t, data)
	ref := resample(clip.Samples, 1, 24000, granuleRate)
	var sig, noise float64
	for i := range min(len(ref), len(d.samples)) {
		e := float64(d.samples[i] - ref[i])
		sig += float64(ref[i]) * float64(ref[i])
		noise += e * e
	}
	if snr := 10 * math.Log10(sig/noise); snr < 10 {
		t.Errorf("speech SNR %.1f dB, want >= 10", snr)
	}
}
