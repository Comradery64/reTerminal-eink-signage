package render

// PackBits implements the canonical Apple/TIFF PackBits RLE. E-paper framebuffers are mostly
// flat fills, so this is both compact and trivially decodable on the MCU with no allocation.
//
// The MCU-side decoder lives in firmware/main/protocol.hpp (mdpf_packbits_decode). Any change
// to the encoding here must be reflected there.
func PackBits(src []byte) []byte {
	out := make([]byte, 0, len(src)/2+16)
	n := len(src)
	i := 0
	for i < n {
		// Count a run of identical bytes.
		runVal := src[i]
		runLen := 1
		for i+runLen < n && src[i+runLen] == runVal && runLen < 128 {
			runLen++
		}
		if runLen >= 2 {
			// Replicate run: control = 1 - runLen (i.e. -(runLen-1)), then the byte.
			out = append(out, byte(257-runLen)) // 256-(runLen-1)
			out = append(out, runVal)
			i += runLen
			continue
		}
		// Literal run: gather bytes until a run of >=3 identical appears or we hit 128.
		litStart := i
		litLen := 0
		for i < n && litLen < 128 {
			// Stop the literal if a 3+ replicate run begins here.
			if i+2 < n && src[i] == src[i+1] && src[i+1] == src[i+2] {
				break
			}
			i++
			litLen++
		}
		out = append(out, byte(litLen-1)) // 0..127 => copy litLen bytes
		out = append(out, src[litStart:litStart+litLen]...)
	}
	return out
}
