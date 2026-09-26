// Command lacbench: "enc <speech|music|auto> in.wav out.lac" / "dec in.lac out.wav".
package main

import (
	"os"

	"github.com/rhino1998/lectable/lac"
)

func main() {
	switch os.Args[1] {
	case "enc":
		p := map[string]lac.Preset{"auto": lac.PresetAuto, "speech": lac.PresetSpeech, "music": lac.PresetMusic}[os.Args[2]]
		wav, err := os.ReadFile(os.Args[3])
		if err != nil {
			panic(err)
		}
		b, err := lac.EncodeWAV(wav, p)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(os.Args[4], b, 0o644); err != nil {
			panic(err)
		}
	case "dec":
		b, err := os.ReadFile(os.Args[2])
		if err != nil {
			panic(err)
		}
		wav, err := lac.DecodeWAV(b)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(os.Args[3], wav, 0o644); err != nil {
			panic(err)
		}
	}
}
