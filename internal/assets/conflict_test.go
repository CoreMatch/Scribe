package assets

import (
	"strings"
	"testing"
)

func TestResolveConflictMatrix(t *testing.T) {
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	tests := []struct {
		name   string
		legacy AssetState
		target AssetState
		want   Decision
	}{
		{name: "legacy only", legacy: AssetState{Present: true, PixelSHA256: first}, want: Upload},
		{name: "same pixels", legacy: AssetState{Present: true, PixelSHA256: first}, target: AssetState{Present: true, PixelSHA256: strings.ToUpper(first)}, want: Unchanged},
		{name: "different pixels", legacy: AssetState{Present: true, PixelSHA256: first}, target: AssetState{Present: true, PixelSHA256: second}, want: Conflict},
		{name: "target only", target: AssetState{Present: true, PixelSHA256: first}, want: Conflict},
		{name: "neither", want: Absent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveConflict(tt.legacy, tt.target)
			if err != nil || got != tt.want {
				t.Fatalf("ResolveConflict() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestResolveConflictRejectsInvalidDigests(t *testing.T) {
	for _, digest := range []string{"", "abc", strings.Repeat("g", 64)} {
		if _, err := ResolveConflict(AssetState{Present: true, PixelSHA256: digest}, AssetState{}); err == nil {
			t.Fatalf("ResolveConflict() accepted digest %q", digest)
		}
	}
}
