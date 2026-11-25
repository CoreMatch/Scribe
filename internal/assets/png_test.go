package assets

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestNormalizePNGUsesCanonicalPixels(t *testing.T) {
	limits := DefaultLimits()
	first := encodePNG(t, &image.NRGBA{
		Pix:    []byte{255, 0, 0, 255, 0, 255, 0, 128, 0, 0, 255, 255, 255, 255, 255, 0},
		Stride: 8,
		Rect:   image.Rect(0, 0, 2, 2),
	})
	palette := color.Palette{
		color.NRGBA{R: 255, A: 255},
		color.NRGBA{G: 255, A: 128},
		color.NRGBA{B: 255, A: 255},
		color.NRGBA{R: 255, G: 255, B: 255},
	}
	second := encodePNG(t, &image.Paletted{
		Palette: palette,
		Pix:     []byte{0, 1, 2, 3},
		Stride:  2,
		Rect:    image.Rect(0, 0, 2, 2),
	})
	second = addPNGText(t, second, "Comment", "metadata must be stripped")

	firstAsset, err := NormalizePNG(first, limits)
	if err != nil {
		t.Fatal(err)
	}
	secondAsset, err := NormalizePNG(second, limits)
	if err != nil {
		t.Fatal(err)
	}
	if firstAsset.PixelSHA256 != secondAsset.PixelSHA256 {
		t.Fatalf("pixel digests differ: %s != %s", firstAsset.PixelSHA256, secondAsset.PixelSHA256)
	}
	if !bytes.Equal(firstAsset.PNG, secondAsset.PNG) {
		t.Fatal("normalized PNG encodings differ")
	}
	for _, chunk := range pngChunks(t, firstAsset.PNG) {
		if string(chunk[0]) == "tEXt" || string(chunk[0]) == "iTXt" || string(chunk[0]) == "eXIf" {
			t.Fatalf("normalized PNG retained metadata chunk %q", chunk[0])
		}
	}

	decoded, err := png.Decode(bytes.NewReader(firstAsset.PNG))
	if err != nil {
		t.Fatal(err)
	}
	nrgba := decoded.(*image.NRGBA)
	want := []byte{255, 0, 0, 255, 0, 255, 0, 128, 0, 0, 255, 255, 0, 0, 0, 0}
	if !bytes.Equal(nrgba.Pix, want) {
		t.Fatalf("RGBA pixels = %v, want %v", nrgba.Pix, want)
	}
}

func TestNormalizePNGIncludesDimensionsInDigest(t *testing.T) {
	horizontal := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 1)))
	vertical := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 1, 2)))
	first, err := NormalizePNG(horizontal, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	second, err := NormalizePNG(vertical, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if first.PixelSHA256 == second.PixelSHA256 {
		t.Fatal("different dimensions produced the same pixel digest")
	}
}

func TestNormalizePNGRejectsInvalidOrOversizedInput(t *testing.T) {
	valid := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	tests := []struct {
		name   string
		data   []byte
		limits Limits
	}{
		{name: "invalid", data: []byte("not a PNG"), limits: DefaultLimits()},
		{name: "encoded size", data: valid, limits: Limits{MaxEncodedBytes: int64(len(valid) - 1), MaxWidth: 2, MaxHeight: 2, MaxPixels: 4}},
		{name: "width", data: valid, limits: Limits{MaxEncodedBytes: 1 << 20, MaxWidth: 1, MaxHeight: 2, MaxPixels: 4}},
		{name: "height", data: valid, limits: Limits{MaxEncodedBytes: 1 << 20, MaxWidth: 2, MaxHeight: 1, MaxPixels: 4}},
		{name: "pixels", data: valid, limits: Limits{MaxEncodedBytes: 1 << 20, MaxWidth: 2, MaxHeight: 2, MaxPixels: 3}},
		{name: "zero limits", data: valid, limits: Limits{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NormalizePNG(tt.data, tt.limits); err == nil {
				t.Fatal("NormalizePNG() unexpectedly succeeded")
			}
		})
	}
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func addPNGText(t *testing.T, data []byte, keyword, text string) []byte {
	t.Helper()
	chunks := pngChunks(t, data)
	var output bytes.Buffer
	output.Write(data[:8])
	for _, chunk := range chunks {
		if string(chunk[0]) != "IEND" {
			writePNGChunk(&output, chunk)
		}
	}
	payload := append([]byte(keyword), 0)
	payload = append(payload, text...)
	writePNGChunk(&output, []byte("tEXt"+string(payload)))
	writePNGChunk(&output, []byte("IEND"))
	return output.Bytes()
}

func pngChunks(t *testing.T, data []byte) [][]byte {
	t.Helper()
	if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("invalid PNG signature")
	}
	var chunks [][]byte
	for offset := 8; offset < len(data); {
		if offset+12 > len(data) {
			t.Fatal("truncated PNG chunk")
		}
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		end := offset + 12 + length
		if end > len(data) {
			t.Fatal("truncated PNG chunk payload")
		}
		chunks = append(chunks, data[offset+4:offset+8+length])
		offset = end
	}
	return chunks
}

func writePNGChunk(output *bytes.Buffer, chunk []byte) {
	payload := chunk[4:]
	_ = binary.Write(output, binary.BigEndian, uint32(len(payload)))
	output.Write(chunk[:4])
	output.Write(payload)
	_ = binary.Write(output, binary.BigEndian, crc32.ChecksumIEEE(chunk))
}
