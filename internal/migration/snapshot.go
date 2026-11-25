package migration

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"

	"github.com/corematch/scribe/internal/assets"
	"github.com/corematch/scribe/internal/protocol"
)

var (
	ErrSnapshotProfile   = errors.New("legacy profile snapshot failed")
	ErrSnapshotAsset     = errors.New("legacy asset snapshot failed")
	ErrProfileMismatch   = errors.New("legacy profile does not match selected profile")
	ErrUnexpectedTexture = errors.New("legacy profile contains an unsupported texture")
)

type Snapshot struct {
	Profile  protocol.SignedProfile
	Textures protocol.Textures
	Assets   map[string]assets.Asset
}

type ProfileSource interface {
	FetchVerificationKey(ctx context.Context) (*rsa.PublicKey, error)
	FetchProfile(ctx context.Context, profileID string, publicKey *rsa.PublicKey) (protocol.SignedProfile, protocol.Textures, bool, error)
}

type TextureSource interface {
	Download(ctx context.Context, rawURL string) ([]byte, error)
}

type Options struct {
	PNGLimits assets.Limits
}

type Snapshotter struct {
	profiles  ProfileSource
	textures  TextureSource
	pngLimits assets.Limits
}

func New(profiles ProfileSource, textures TextureSource, options Options) (*Snapshotter, error) {
	if profiles == nil || textures == nil {
		return nil, errors.New("profile and texture sources are required")
	}
	limits := options.PNGLimits
	if limits == (assets.Limits{}) {
		limits = assets.DefaultLimits()
	}
	if limits.MaxEncodedBytes <= 0 || limits.MaxWidth <= 0 || limits.MaxHeight <= 0 || limits.MaxPixels <= 0 {
		return nil, errors.New("PNG limits must be positive")
	}
	return &Snapshotter{profiles: profiles, textures: textures, pngLimits: limits}, nil
}

func (s *Snapshotter) Capture(ctx context.Context, selected protocol.Profile) (Snapshot, error) {
	if !protocol.ValidProfileID(selected.ID) || selected.Name == "" {
		return Snapshot{}, ErrProfileMismatch
	}
	publicKey, err := s.profiles.FetchVerificationKey(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrSnapshotProfile, err)
	}
	profile, textures, present, err := s.profiles.FetchProfile(ctx, selected.ID, publicKey)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrSnapshotProfile, err)
	}
	if !strings.EqualFold(profile.ID, selected.ID) || profile.Name != selected.Name {
		return Snapshot{}, ErrProfileMismatch
	}
	if !present {
		return Snapshot{Profile: profile, Textures: textures, Assets: map[string]assets.Asset{}}, nil
	}
	for name := range textures.Textures {
		if name != "SKIN" && name != "CAPE" {
			return Snapshot{}, ErrUnexpectedTexture
		}
	}

	normalized := make(map[string]assets.Asset, len(textures.Textures))
	for _, name := range []string{"SKIN", "CAPE"} {
		texture, ok := textures.Textures[name]
		if !ok {
			continue
		}
		data, err := s.textures.Download(ctx, texture.URL)
		if err != nil {
			return Snapshot{}, fmt.Errorf("%w: %w", ErrSnapshotAsset, err)
		}
		asset, err := assets.NormalizePNG(data, s.pngLimits)
		if err != nil {
			return Snapshot{}, fmt.Errorf("%w: %w", ErrSnapshotAsset, err)
		}
		normalized[name] = asset
	}
	return Snapshot{Profile: profile, Textures: textures, Assets: normalized}, nil
}
