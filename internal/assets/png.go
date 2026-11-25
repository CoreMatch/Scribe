package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

type Limits struct {
	MaxEncodedBytes int64
	MaxWidth        int
	MaxHeight       int
	MaxPixels       int
}

type Asset struct {
	PNG         []byte
	PixelSHA256 string
	Width       int
	Height      int
}

func DefaultLimits() Limits {
	return Limits{
		MaxEncodedBytes: 4 << 20,
		MaxWidth:        4096,
		MaxHeight:       4096,
		MaxPixels:       16 << 20,
	}
}

func NormalizePNG(data []byte, limits Limits) (Asset, error) {
	if limits.MaxEncodedBytes <= 0 || limits.MaxWidth <= 0 || limits.MaxHeight <= 0 || limits.MaxPixels <= 0 {
		return Asset{}, fmt.Errorf("PNG limits must be positive")
	}
	if int64(len(data)) > limits.MaxEncodedBytes {
		return Asset{}, fmt.Errorf("PNG exceeds encoded size limit")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Asset{}, fmt.Errorf("invalid PNG: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > limits.MaxWidth || config.Height > limits.MaxHeight {
		return Asset{}, fmt.Errorf("PNG dimensions exceed limit")
	}
	if config.Width > limits.MaxPixels/config.Height {
		return Asset{}, fmt.Errorf("PNG pixel count exceeds limit")
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return Asset{}, fmt.Errorf("invalid PNG: %w", err)
	}
	bounds := decoded.Bounds()
	normalized := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(normalized, normalized.Bounds(), decoded, bounds.Min, draw.Src)
	for offset := 3; offset < len(normalized.Pix); offset += 4 {
		if normalized.Pix[offset] == 0 {
			normalized.Pix[offset-3] = 0
			normalized.Pix[offset-2] = 0
			normalized.Pix[offset-1] = 0
		}
	}

	digest := sha256.New()
	_ = binary.Write(digest, binary.BigEndian, uint32(bounds.Dx()))
	_ = binary.Write(digest, binary.BigEndian, uint32(bounds.Dy()))
	_, _ = digest.Write(normalized.Pix)

	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := encoder.Encode(&encoded, normalized); err != nil {
		return Asset{}, fmt.Errorf("encode normalized PNG: %w", err)
	}
	if int64(encoded.Len()) > limits.MaxEncodedBytes {
		return Asset{}, fmt.Errorf("normalized PNG exceeds encoded size limit")
	}
	return Asset{
		PNG:         encoded.Bytes(),
		PixelSHA256: hex.EncodeToString(digest.Sum(nil)),
		Width:       bounds.Dx(),
		Height:      bounds.Dy(),
	}, nil
}
