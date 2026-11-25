package protocol

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testProfileID = "12345678123412341234567812345678"

func TestDecodeSignedProfile(t *testing.T) {
	profile, err := DecodeSignedProfile([]byte(`{"id":"12345678123412341234567812345678","name":"LegacyPlayer","properties":[]}`))
	if err != nil || profile.ID != testProfileID || profile.Name != "LegacyPlayer" {
		t.Fatalf("DecodeSignedProfile() = %+v, %v", profile, err)
	}
	for _, body := range []string{
		``,
		`not-json`,
		`{}`,
		`{"id":"invalid","name":"LegacyPlayer"}`,
		`{"id":"12345678123412341234567812345678","name":" "}`,
	} {
		if _, err := DecodeSignedProfile([]byte(body)); err == nil {
			t.Fatalf("DecodeSignedProfile(%q) unexpectedly succeeded", body)
		}
	}
	if _, err := DecodeSignedProfile(make([]byte, maxProfileSize+1)); err == nil {
		t.Fatal("DecodeSignedProfile() accepted an oversized profile")
	}
}

func TestRewriteAndVerifyTextures(t *testing.T) {
	privateKey := testRSAKey(t, 2048)
	profile := SignedProfile{
		ID:   testProfileID,
		Name: "LegacyPlayer",
		Properties: []ProfileProperty{
			{Name: "custom", Value: "preserved", Signature: "preserved-signature"},
		},
	}
	textures := Textures{Textures: map[string]Texture{
		"SKIN": {URL: "https://textures.example.com/skin.png", Metadata: map[string]string{"model": "slim"}},
		"CAPE": {URL: "https://textures.example.com/cape.png"},
	}}
	now := time.UnixMilli(1_700_000_000_123)
	updated, err := RewriteTextures(profile, textures, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Properties) != 2 || updated.Properties[0] != profile.Properties[0] {
		t.Fatalf("non-texture properties changed: %+v", updated.Properties)
	}
	verified, present, err := VerifiedTextures(updated, &privateKey.PublicKey)
	if err != nil || !present {
		t.Fatalf("VerifiedTextures() present=%v error=%v", present, err)
	}
	if verified.Timestamp != now.UnixMilli() || verified.ProfileID != profile.ID || verified.ProfileName != profile.Name || !reflect.DeepEqual(verified.Textures, textures.Textures) {
		t.Fatalf("verified textures = %+v", verified)
	}

	encoded, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SignedProfile
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifiedTextures(decoded, &privateKey.PublicKey); err != nil {
		t.Fatalf("serialized profile verification failed: %v", err)
	}
}

func TestVerifiedTexturesRejectsInvalidSignaturesAndIdentity(t *testing.T) {
	privateKey := testRSAKey(t, 2048)
	otherKey := testRSAKey(t, 2048)
	profile := SignedProfile{ID: testProfileID, Name: "LegacyPlayer"}
	textures := Textures{Textures: map[string]Texture{"SKIN": {URL: "https://textures.example.com/skin.png"}}}
	signed, err := RewriteTextures(profile, textures, privateKey, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	property := signed.Properties[0]

	tamperedValue := property
	value, _ := base64.StdEncoding.DecodeString(property.Value)
	var payload map[string]any
	_ = json.Unmarshal(value, &payload)
	payload["profileName"] = "OtherPlayer"
	value, _ = json.Marshal(payload)
	tamperedValue.Value = base64.StdEncoding.EncodeToString(value)

	tamperedSignature := property
	signature, _ := base64.StdEncoding.DecodeString(property.Signature)
	signature[0] ^= 0xff
	tamperedSignature.Signature = base64.StdEncoding.EncodeToString(signature)

	tests := []struct {
		name    string
		profile SignedProfile
		key     *rsa.PublicKey
	}{
		{name: "wrong key", profile: signed, key: &otherKey.PublicKey},
		{name: "tampered value", profile: SignedProfile{ID: profile.ID, Name: profile.Name, Properties: []ProfileProperty{tamperedValue}}, key: &privateKey.PublicKey},
		{name: "tampered signature", profile: SignedProfile{ID: profile.ID, Name: profile.Name, Properties: []ProfileProperty{tamperedSignature}}, key: &privateKey.PublicKey},
		{name: "unsigned", profile: SignedProfile{ID: profile.ID, Name: profile.Name, Properties: []ProfileProperty{{Name: TexturesProperty, Value: property.Value}}}, key: &privateKey.PublicKey},
		{name: "duplicate", profile: SignedProfile{ID: profile.ID, Name: profile.Name, Properties: []ProfileProperty{property, property}}, key: &privateKey.PublicKey},
		{name: "profile mismatch", profile: SignedProfile{ID: strings.Repeat("a", 32), Name: profile.Name, Properties: signed.Properties}, key: &privateKey.PublicKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := VerifiedTextures(tt.profile, tt.key); err == nil {
				t.Fatal("VerifiedTextures() unexpectedly succeeded")
			}
		})
	}
}

func TestVerifiedTexturesSignsEncodedPropertyValue(t *testing.T) {
	key := testRSAKey(t, 2048)
	payload := Textures{
		Timestamp:   1_700_000_000_000,
		ProfileID:   testProfileID,
		ProfileName: "LegacyPlayer",
		Textures:    map[string]Texture{"SKIN": {URL: "https://textures.example.com/skin.png"}},
	}
	value, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(value)
	digest := sha1.Sum([]byte(encoded))
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA1, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	profile := SignedProfile{
		ID:   testProfileID,
		Name: "LegacyPlayer",
		Properties: []ProfileProperty{{
			Name:      TexturesProperty,
			Value:     encoded,
			Signature: base64.StdEncoding.EncodeToString(signature),
		}},
	}
	if _, present, err := VerifiedTextures(profile, &key.PublicKey); err != nil || !present {
		t.Fatalf("VerifiedTextures() present=%v error=%v", present, err)
	}
}

func TestVerifiedTexturesAllowsAbsentProperty(t *testing.T) {
	key := testRSAKey(t, 2048)
	profile := SignedProfile{ID: testProfileID, Name: "LegacyPlayer"}
	textures, present, err := VerifiedTextures(profile, &key.PublicKey)
	if err != nil || present || textures.Textures == nil || len(textures.Textures) != 0 {
		t.Fatalf("VerifiedTextures() = %+v, %v, %v", textures, present, err)
	}
}

func TestRewriteTexturesRejectsInvalidPayloads(t *testing.T) {
	key := testRSAKey(t, 2048)
	profile := SignedProfile{ID: testProfileID, Name: "LegacyPlayer"}
	tests := []struct {
		name     string
		textures Textures
	}{
		{name: "profile ID", textures: Textures{ProfileID: "not-a-uuid"}},
		{name: "profile name", textures: Textures{ProfileName: "OtherPlayer"}},
		{name: "relative URL", textures: Textures{Textures: map[string]Texture{"SKIN": {URL: "/skin.png"}}}},
		{name: "credentials in URL", textures: Textures{Textures: map[string]Texture{"SKIN": {URL: "https://user:pass@example.com/skin.png"}}}},
		{name: "fragment in URL", textures: Textures{Textures: map[string]Texture{"SKIN": {URL: "https://example.com/skin.png#fragment"}}}},
		{name: "invalid model", textures: Textures{Textures: map[string]Texture{"SKIN": {URL: "https://example.com/skin.png", Metadata: map[string]string{"model": "unknown"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := RewriteTextures(profile, tt.textures, key, time.UnixMilli(1_700_000_000_000)); err == nil {
				t.Fatal("RewriteTextures() unexpectedly succeeded")
			}
		})
	}
}

func TestSignatureKeyParsing(t *testing.T) {
	key := testRSAKey(t, 2048)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	parsedPublic, err := ParsePublicKey(publicPEM)
	if err != nil || parsedPublic.N.Cmp(key.N) != 0 {
		t.Fatalf("ParsePublicKey() = %v, %v", parsedPublic, err)
	}
	parsedPrivate, err := ParsePrivateKey(privatePEM)
	if err != nil || parsedPrivate.D.Cmp(key.D) != 0 {
		t.Fatalf("ParsePrivateKey() = %v, %v", parsedPrivate, err)
	}
	for _, data := range [][]byte{nil, []byte("not PEM"), publicPEM} {
		if _, err := ParsePrivateKey(data); err == nil {
			t.Fatal("ParsePrivateKey() accepted invalid private key")
		}
	}
	if _, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testRSAKey(t, 1024))})); err == nil {
		t.Fatal("ParsePrivateKey() accepted a weak RSA key")
	}
}

func testRSAKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
