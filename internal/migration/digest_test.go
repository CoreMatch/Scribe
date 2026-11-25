package migration

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/corematch/scribe/internal/assets"
	"github.com/corematch/scribe/internal/protocol"
)

func TestCommitDigestCanonicalizesTargetState(t *testing.T) {
	identifierHash := strings.Repeat("ab", 32)
	snapshot := validDigestSnapshot(t)
	first, err := CommitDigest(snapshot, identifierHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("digest length = %d", len(first))
	}

	changedSource := validDigestSnapshot(t)
	changedSource.Textures.Timestamp++
	skin := changedSource.Textures.Textures["SKIN"]
	skin.URL = "https://textures.example/new-skin.png?token=different"
	changedSource.Textures.Textures["SKIN"] = skin
	changedSource.Profile.ID = strings.ToUpper(changedSource.Profile.ID)
	changedSource.Textures.ProfileID = changedSource.Profile.ID
	second, err := CommitDigest(changedSource, strings.ToUpper(identifierHash))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("equivalent target state produced %s and %s", first, second)
	}
}

func TestCommitDigestChangesForTargetStateChanges(t *testing.T) {
	identifierHash := strings.Repeat("ab", 32)
	base := validDigestSnapshot(t)
	want, err := CommitDigest(base, identifierHash)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "profile name", mutate: func(s *Snapshot) {
			s.Profile.Name = "RenamedPlayer"
			s.Textures.ProfileName = s.Profile.Name
		}},
		{name: "skin model", mutate: func(s *Snapshot) {
			s.Textures.Textures["SKIN"].Metadata["model"] = "default"
		}},
		{name: "asset bytes", mutate: func(s *Snapshot) {
			asset := s.Assets["SKIN"]
			asset.PNG = append(append([]byte(nil), asset.PNG...), 0)
			s.Assets["SKIN"] = asset
		}},
		{name: "asset dimensions", mutate: func(s *Snapshot) {
			asset := s.Assets["SKIN"]
			asset.Width++
			s.Assets["SKIN"] = asset
		}},
		{name: "missing cape", mutate: func(s *Snapshot) {
			delete(s.Assets, "CAPE")
			delete(s.Textures.Textures, "CAPE")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := validDigestSnapshot(t)
			tt.mutate(&snapshot)
			got, err := CommitDigest(snapshot, identifierHash)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("target state change did not change commit digest")
			}
		})
	}
	changedIdentifier := validDigestSnapshot(t)
	got, err := CommitDigest(changedIdentifier, strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatal("identifier change did not change commit digest")
	}
}

func TestCommitDigestRejectsInvalidSnapshots(t *testing.T) {
	identifierHash := strings.Repeat("ab", 32)
	tests := []struct {
		name   string
		mutate func(*Snapshot)
		hash   string
	}{
		{name: "invalid identifier hash", hash: "not-a-hash", mutate: func(*Snapshot) {}},
		{name: "invalid profile ID", mutate: func(s *Snapshot) { s.Profile.ID = "invalid" }},
		{name: "profile mismatch", mutate: func(s *Snapshot) { s.Textures.ProfileName = "OtherPlayer" }},
		{name: "missing asset", mutate: func(s *Snapshot) { delete(s.Assets, "SKIN") }},
		{name: "extra asset", mutate: func(s *Snapshot) { s.Assets["OTHER"] = s.Assets["SKIN"] }},
		{name: "unsupported texture", mutate: func(s *Snapshot) {
			s.Textures.Textures["OTHER"] = protocol.Texture{URL: "https://textures.example/other.png"}
			s.Assets["OTHER"] = s.Assets["SKIN"]
		}},
		{name: "invalid pixel digest", mutate: func(s *Snapshot) {
			asset := s.Assets["SKIN"]
			asset.PixelSHA256 = "invalid"
			s.Assets["SKIN"] = asset
		}},
		{name: "empty PNG", mutate: func(s *Snapshot) {
			asset := s.Assets["SKIN"]
			asset.PNG = nil
			s.Assets["SKIN"] = asset
		}},
		{name: "invalid dimensions", mutate: func(s *Snapshot) {
			asset := s.Assets["SKIN"]
			asset.Height = 0
			s.Assets["SKIN"] = asset
		}},
		{name: "unknown texture metadata", mutate: func(s *Snapshot) {
			skin := s.Textures.Textures["SKIN"]
			skin.Metadata = map[string]string{"model": "slim", "password": "secret"}
			s.Textures.Textures["SKIN"] = skin
		}},
		{name: "invalid texture model", mutate: func(s *Snapshot) {
			skin := s.Textures.Textures["SKIN"]
			skin.Metadata = map[string]string{"model": "unknown"}
			s.Textures.Textures["SKIN"] = skin
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := validDigestSnapshot(t)
			tt.mutate(&snapshot)
			hash := tt.hash
			if hash == "" {
				hash = identifierHash
			}
			if _, err := CommitDigest(snapshot, hash); err == nil {
				t.Fatal("CommitDigest() unexpectedly succeeded")
			}
		})
	}
}

func TestCommitDigestAllowsEmptyAssetSet(t *testing.T) {
	snapshot := Snapshot{
		Profile: protocol.SignedProfile{ID: snapshotProfileID, Name: "LegacyPlayer"},
		Textures: protocol.Textures{
			ProfileID:   snapshotProfileID,
			ProfileName: "LegacyPlayer",
			Textures:    map[string]protocol.Texture{},
		},
		Assets: map[string]assets.Asset{},
	}
	if digest, err := CommitDigest(snapshot, strings.Repeat("ab", 32)); err != nil || len(digest) != 64 {
		t.Fatalf("CommitDigest() = %q, %v", digest, err)
	}
}

func validDigestSnapshot(t *testing.T) Snapshot {
	t.Helper()
	skin, err := assets.NormalizePNG(digestTestPNG(t, 2, 1), assets.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cape, err := assets.NormalizePNG(digestTestPNG(t, 3, 1), assets.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return Snapshot{
		Profile: protocol.SignedProfile{ID: snapshotProfileID, Name: "LegacyPlayer"},
		Textures: protocol.Textures{
			Timestamp:   1_700_000_000_000,
			ProfileID:   snapshotProfileID,
			ProfileName: "LegacyPlayer",
			Textures: map[string]protocol.Texture{
				"SKIN": {URL: "https://textures.example/skin.png?token=secret", Metadata: map[string]string{"model": "slim"}},
				"CAPE": {URL: "https://textures.example/cape.png"},
			},
		},
		Assets: map[string]assets.Asset{"SKIN": skin, "CAPE": cape},
	}
}

func digestTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(10 + x), G: uint8(20 + y), B: 30, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
