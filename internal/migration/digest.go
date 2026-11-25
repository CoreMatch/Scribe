package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/corematch/scribe/internal/protocol"
)

var ErrInvalidSnapshot = errors.New("invalid migration snapshot")

type commitSummary struct {
	Version          int                 `json:"version"`
	IdentifierSHA256 string              `json:"identifier_sha256"`
	ProfileID        string              `json:"profile_id"`
	ProfileName      string              `json:"profile_name"`
	SkinSync         bool                `json:"skin_sync"`
	Skin             *commitAssetSummary `json:"skin,omitempty"`
	Cape             *commitAssetSummary `json:"cape,omitempty"`
}

type commitAssetSummary struct {
	PixelSHA256 string            `json:"pixel_sha256"`
	PNGSHA256   string            `json:"png_sha256"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func CommitDigest(snapshot Snapshot, identifierHash string) (string, error) {
	identifierHash, err := canonicalSHA256(identifierHash)
	if err != nil {
		return "", fmt.Errorf("%w: identifier hash", ErrInvalidSnapshot)
	}
	summary, err := summarize(snapshot, identifierHash)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return "", fmt.Errorf("%w: encode commit summary", ErrInvalidSnapshot)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("scribe-migration-v1\x00"))
	_, _ = digest.Write(encoded)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func summarize(snapshot Snapshot, identifierHash string) (commitSummary, error) {
	profile := snapshot.Profile
	textures := snapshot.Textures
	if !validSnapshotIdentity(profile, textures) || len(snapshot.Assets) != len(textures.Textures) {
		return commitSummary{}, fmt.Errorf("%w: profile identity or asset set", ErrInvalidSnapshot)
	}
	summary := commitSummary{
		Version:          1,
		IdentifierSHA256: identifierHash,
		ProfileID:        strings.ToLower(profile.ID),
		ProfileName:      profile.Name,
		SkinSync:         false,
	}
	for name, texture := range textures.Textures {
		if name != "SKIN" && name != "CAPE" {
			return commitSummary{}, fmt.Errorf("%w: unsupported texture", ErrInvalidSnapshot)
		}
		asset, ok := snapshot.Assets[name]
		if !ok {
			return commitSummary{}, fmt.Errorf("%w: missing asset", ErrInvalidSnapshot)
		}
		pixelHash, err := canonicalSHA256(asset.PixelSHA256)
		if err != nil || len(asset.PNG) == 0 || asset.Width <= 0 || asset.Height <= 0 {
			return commitSummary{}, fmt.Errorf("%w: invalid asset", ErrInvalidSnapshot)
		}
		pngHash := sha256.Sum256(asset.PNG)
		assetSummary := &commitAssetSummary{
			PixelSHA256: pixelHash,
			PNGSHA256:   hex.EncodeToString(pngHash[:]),
			Width:       asset.Width,
			Height:      asset.Height,
			Metadata:    texture.Metadata,
		}
		if name == "SKIN" {
			summary.Skin = assetSummary
		} else {
			summary.Cape = assetSummary
		}
	}
	return summary, nil
}

func validSnapshotIdentity(profile protocol.SignedProfile, textures protocol.Textures) bool {
	return protocol.ValidProfileID(profile.ID) && profile.Name != "" &&
		strings.EqualFold(profile.ID, textures.ProfileID) && profile.Name == textures.ProfileName
}

func canonicalSHA256(value string) (string, error) {
	if len(value) != sha256.Size*2 {
		return "", errors.New("invalid SHA-256 hex string")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(decoded), nil
}
