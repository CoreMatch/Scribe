package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type Decision string

const (
	Upload    Decision = "upload"
	Unchanged Decision = "unchanged"
	Absent    Decision = "absent"
	Conflict  Decision = "conflict"
)

type AssetState struct {
	Present     bool
	PixelSHA256 string
}

func ResolveConflict(legacy, target AssetState) (Decision, error) {
	if err := validateState(legacy); err != nil {
		return "", fmt.Errorf("legacy asset: %w", err)
	}
	if err := validateState(target); err != nil {
		return "", fmt.Errorf("target asset: %w", err)
	}
	switch {
	case !legacy.Present && target.Present:
		return Conflict, nil
	case !legacy.Present:
		return Absent, nil
	case !target.Present:
		return Upload, nil
	case strings.EqualFold(legacy.PixelSHA256, target.PixelSHA256):
		return Unchanged, nil
	default:
		return Conflict, nil
	}
}

func validateState(state AssetState) error {
	if !state.Present {
		return nil
	}
	if len(state.PixelSHA256) != sha256.Size*2 {
		return fmt.Errorf("pixel digest must be a SHA-256 hex string")
	}
	_, err := hex.DecodeString(state.PixelSHA256)
	return err
}
