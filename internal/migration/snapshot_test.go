package migration

import (
	"bytes"
	"context"
	"crypto/rsa"
	"errors"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"testing"

	"github.com/corematch/scribe/internal/assets"
	"github.com/corematch/scribe/internal/protocol"
)

const snapshotProfileID = "12345678123412341234567812345678"

func TestCaptureBuildsNormalizedSnapshot(t *testing.T) {
	skinPNG := testPNG(t)
	capePNG := testPNG(t)
	profiles := &stubProfiles{
		profile: protocol.SignedProfile{ID: snapshotProfileID, Name: "LegacyPlayer"},
		textures: protocol.Textures{
			ProfileID:   snapshotProfileID,
			ProfileName: "LegacyPlayer",
			Textures: map[string]protocol.Texture{
				"SKIN": {URL: "https://textures.example/skin.png?token=secret", Metadata: map[string]string{"model": "slim"}},
				"CAPE": {URL: "https://textures.example/cape.png"},
			},
		},
		present: true,
	}
	textures := &stubTextures{data: map[string][]byte{
		"https://textures.example/skin.png?token=secret": skinPNG,
		"https://textures.example/cape.png":              capePNG,
	}}
	snapshotter, err := New(profiles, textures, Options{})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := snapshotter.Capture(context.Background(), protocol.Profile{ID: snapshotProfileID, Name: "LegacyPlayer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Assets) != 2 || snapshot.Textures.Textures["SKIN"].Metadata["model"] != "slim" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	for name, source := range map[string][]byte{"SKIN": skinPNG, "CAPE": capePNG} {
		want, err := assets.NormalizePNG(source, assets.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		got := snapshot.Assets[name]
		if got.PixelSHA256 != want.PixelSHA256 || got.Width != want.Width || got.Height != want.Height || !bytes.Equal(got.PNG, want.PNG) {
			t.Fatalf("asset %s = %+v, want %+v", name, got, want)
		}
	}
	wantDownloads := []string{"https://textures.example/skin.png?token=secret", "https://textures.example/cape.png"}
	if !reflect.DeepEqual(textures.calls, wantDownloads) {
		t.Fatalf("downloads = %v, want %v", textures.calls, wantDownloads)
	}
}

func TestCaptureReturnsNoPartialSnapshotOnAssetFailure(t *testing.T) {
	skinURL := "https://textures.example/skin.png"
	capeURL := "https://textures.example/cape.png"
	tests := []struct {
		name      string
		capeData  []byte
		capeError error
	}{
		{name: "download failure", capeError: errors.New("download failed")},
		{name: "invalid PNG", capeData: []byte("not a PNG")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profiles := profilesWithTextures(map[string]protocol.Texture{
				"SKIN": {URL: skinURL},
				"CAPE": {URL: capeURL},
			})
			textures := &stubTextures{
				data: map[string][]byte{skinURL: testPNG(t), capeURL: tt.capeData},
				errs: map[string]error{capeURL: tt.capeError},
			}
			snapshotter, err := New(profiles, textures, Options{})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := snapshotter.Capture(context.Background(), protocol.Profile{ID: snapshotProfileID, Name: "LegacyPlayer"})
			if !errors.Is(err, ErrSnapshotAsset) {
				t.Fatalf("Capture() error = %v", err)
			}
			if !reflect.DeepEqual(snapshot, Snapshot{}) {
				t.Fatalf("Capture() returned a partial snapshot: %+v", snapshot)
			}
		})
	}
}

func TestCaptureRejectsUnsupportedTexturesBeforeDownload(t *testing.T) {
	profiles := profilesWithTextures(map[string]protocol.Texture{
		"SKIN":  {URL: "https://textures.example/skin.png"},
		"OTHER": {URL: "https://textures.example/other.png"},
	})
	textures := &stubTextures{}
	snapshotter, err := New(profiles, textures, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotter.Capture(context.Background(), protocol.Profile{ID: snapshotProfileID, Name: "LegacyPlayer"}); !errors.Is(err, ErrUnexpectedTexture) {
		t.Fatalf("Capture() error = %v", err)
	}
	if len(textures.calls) != 0 {
		t.Fatalf("unexpected downloads: %v", textures.calls)
	}
}

func TestCaptureWithoutTexturesReturnsEmptySnapshot(t *testing.T) {
	profiles := &stubProfiles{
		profile:  protocol.SignedProfile{ID: snapshotProfileID, Name: "LegacyPlayer"},
		textures: protocol.Textures{ProfileID: snapshotProfileID, ProfileName: "LegacyPlayer", Textures: map[string]protocol.Texture{}},
	}
	textures := &stubTextures{}
	snapshotter, err := New(profiles, textures, Options{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotter.Capture(context.Background(), protocol.Profile{ID: snapshotProfileID, Name: "LegacyPlayer"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Assets == nil || len(snapshot.Assets) != 0 || len(textures.calls) != 0 {
		t.Fatalf("snapshot = %+v, downloads = %v", snapshot, textures.calls)
	}
}

func TestCaptureRejectsProfileFailuresAndMismatches(t *testing.T) {
	t.Run("key failure", func(t *testing.T) {
		profiles := &stubProfiles{keyError: errors.New("key unavailable")}
		snapshotter, err := New(profiles, &stubTextures{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := snapshotter.Capture(context.Background(), protocol.Profile{ID: snapshotProfileID, Name: "LegacyPlayer"}); !errors.Is(err, ErrSnapshotProfile) {
			t.Fatalf("Capture() error = %v", err)
		}
	})
	t.Run("signed profile failure", func(t *testing.T) {
		profiles := &stubProfiles{fetchError: errors.New("signature invalid")}
		snapshotter, err := New(profiles, &stubTextures{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := snapshotter.Capture(context.Background(), protocol.Profile{ID: snapshotProfileID, Name: "LegacyPlayer"}); !errors.Is(err, ErrSnapshotProfile) {
			t.Fatalf("Capture() error = %v", err)
		}
	})
	for _, selected := range []protocol.Profile{
		{ID: "abcdefabcdefabcdefabcdefabcdefab", Name: "LegacyPlayer"},
		{ID: snapshotProfileID, Name: "DifferentPlayer"},
		{ID: "invalid", Name: "LegacyPlayer"},
	} {
		t.Run(selected.ID+"/"+selected.Name, func(t *testing.T) {
			profiles := profilesWithTextures(nil)
			snapshotter, err := New(profiles, &stubTextures{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := snapshotter.Capture(context.Background(), selected); !errors.Is(err, ErrProfileMismatch) {
				t.Fatalf("Capture() error = %v", err)
			}
		})
	}
}

func TestCapturePreservesContextCancellation(t *testing.T) {
	profiles := profilesWithTextures(map[string]protocol.Texture{"SKIN": {URL: "https://textures.example/skin.png"}})
	textures := &stubTextures{errs: map[string]error{"https://textures.example/skin.png": context.Canceled}}
	snapshotter, err := New(profiles, textures, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotter.Capture(context.Background(), protocol.Profile{ID: snapshotProfileID, Name: "LegacyPlayer"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Capture() error = %v", err)
	}
}

func TestNewSnapshotterRejectsInvalidInputs(t *testing.T) {
	profiles := &stubProfiles{}
	textures := &stubTextures{}
	for _, options := range []Options{
		{PNGLimits: assets.Limits{MaxEncodedBytes: -1}},
		{PNGLimits: assets.Limits{MaxEncodedBytes: 1, MaxWidth: 1, MaxHeight: 1, MaxPixels: -1}},
	} {
		if _, err := New(profiles, textures, options); err == nil {
			t.Fatalf("New(%+v) unexpectedly succeeded", options)
		}
	}
	if _, err := New(nil, textures, Options{}); err == nil {
		t.Fatal("New() accepted a nil profile source")
	}
	if _, err := New(profiles, nil, Options{}); err == nil {
		t.Fatal("New() accepted a nil texture source")
	}
}

type stubProfiles struct {
	key        *rsa.PublicKey
	profile    protocol.SignedProfile
	textures   protocol.Textures
	present    bool
	keyError   error
	fetchError error
}

func (s *stubProfiles) FetchVerificationKey(context.Context) (*rsa.PublicKey, error) {
	return s.key, s.keyError
}

func (s *stubProfiles) FetchProfile(context.Context, string, *rsa.PublicKey) (protocol.SignedProfile, protocol.Textures, bool, error) {
	return s.profile, s.textures, s.present, s.fetchError
}

type stubTextures struct {
	data  map[string][]byte
	errs  map[string]error
	calls []string
}

func (s *stubTextures) Download(_ context.Context, rawURL string) ([]byte, error) {
	s.calls = append(s.calls, rawURL)
	if err := s.errs[rawURL]; err != nil {
		return nil, err
	}
	data, ok := s.data[rawURL]
	if !ok {
		return nil, errors.New("unexpected texture URL")
	}
	return data, nil
}

func profilesWithTextures(textures map[string]protocol.Texture) *stubProfiles {
	return &stubProfiles{
		profile: protocol.SignedProfile{ID: snapshotProfileID, Name: "LegacyPlayer"},
		textures: protocol.Textures{
			ProfileID:   snapshotProfileID,
			ProfileName: "LegacyPlayer",
			Textures:    textures,
		},
		present: true,
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	img.SetNRGBA(1, 0, color.NRGBA{R: 100, G: 80, B: 60})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
