// Command photogen writes a photo-like JPEG for measurements: value noise
// summed over octaves with a power-law falloff, so there is detail at every
// scale and a resized WebP takes about as many bytes as one of a photograph.
// Pure noise would not shrink like a photo, and the test project rejects
// random binaries at upload. Phase 2 used:
//
//	go run ./spikes/photogen -beta 0.3 -q 95 -seed 1 -o hero.jpg
package main

import (
	"flag"
	"image"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"os"
)

func main() {
	w := flag.Int("w", 6000, "width")
	h := flag.Int("h", 4000, "height")
	q := flag.Int("q", 90, "JPEG quality")
	beta := flag.Float64("beta", 0.9, "amplitude falloff exponent per octave scale")
	seed := flag.Uint64("seed", 1, "seed")
	out := flag.String("o", "hero.jpg", "output")
	flag.Parse()
	r := rand.New(rand.NewPCG(*seed, 2))
	img := image.NewRGBA(image.Rect(0, 0, *w, *h))
	const octaves = 12
	var chans [3][]float32
	for c := range chans {
		acc := make([]float32, *w**h)
		total := 0.0
		for k := 0; k < octaves; k++ {
			s := math.Pow(2, float64(octaves-1-k))
			amp := math.Pow(s, *beta)
			total += amp
			gw, gh := int(float64(*w)/s)+2, int(float64(*h)/s)+2
			g := make([]float32, gw*gh)
			for i := range g {
				g[i] = float32(r.Float64()*2 - 1)
			}
			for y := 0; y < *h; y++ {
				fy := float64(y) / s
				y0 := int(fy)
				ty := float32(fy - float64(y0))
				for x := 0; x < *w; x++ {
					fx := float64(x) / s
					x0 := int(fx)
					tx := float32(fx - float64(x0))
					a := g[y0*gw+x0]*(1-tx) + g[y0*gw+x0+1]*tx
					b := g[(y0+1)*gw+x0]*(1-tx) + g[(y0+1)*gw+x0+1]*tx
					acc[y*(*w)+x] += float32(amp) * (a*(1-ty) + b*ty)
				}
			}
		}
		for i := range acc {
			acc[i] /= float32(total)
		}
		chans[c] = acc
	}
	base := [3]float32{120, 115, 100}
	for i := 0; i < *w**h; i++ {
		for c := 0; c < 3; c++ {
			v := base[c] + 260*chans[c][i] + 120*chans[0][i]
			img.Pix[i*4+c] = uint8(max(0, min(255, v)))
		}
		img.Pix[i*4+3] = 255
	}
	f, err := os.Create(*out)
	if err != nil {
		panic(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: *q}); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
}
