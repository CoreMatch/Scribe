package protocol

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	TexturesProperty = "textures"
	maxTexturesSize  = 64 << 10
	maxProfileSize   = 1 << 20
)

type ProfileProperty struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Signature string `json:"signature,omitempty"`
}

type SignedProfile struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Properties []ProfileProperty `json:"properties,omitempty"`
}

type Textures struct {
	Timestamp   int64              `json:"timestamp"`
	ProfileID   string             `json:"profileId"`
	ProfileName string             `json:"profileName"`
	Textures    map[string]Texture `json:"textures"`
}

type Texture struct {
	URL      string            `json:"url"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func ParsePublicKey(data []byte) (*rsa.PublicKey, error) {
	key, err := parsePEMKey(data)
	if err != nil {
		return nil, err
	}
	switch key := key.(type) {
	case *rsa.PublicKey:
		return validatePublicKey(key)
	case *rsa.PrivateKey:
		return validatePublicKey(&key.PublicKey)
	default:
		return nil, errors.New("signature key must be RSA")
	}
}

func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	key, err := parsePEMKey(data)
	if err != nil {
		return nil, err
	}
	privateKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("signature key must be an RSA private key")
	}
	if err := privateKey.Validate(); err != nil || privateKey.N.BitLen() < 2048 {
		return nil, errors.New("invalid RSA private key")
	}
	return privateKey, nil
}

func DecodeSignedProfile(data []byte) (SignedProfile, error) {
	if len(data) == 0 || len(data) > maxProfileSize {
		return SignedProfile{}, errors.New("invalid signed profile size")
	}
	var profile SignedProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return SignedProfile{}, errors.New("invalid signed profile JSON")
	}
	if err := validateProfile(profile); err != nil {
		return SignedProfile{}, err
	}
	return profile, nil
}

func VerifiedTextures(profile SignedProfile, publicKey *rsa.PublicKey) (Textures, bool, error) {
	if err := validateProfile(profile); err != nil {
		return Textures{}, false, err
	}
	if _, err := validatePublicKey(publicKey); err != nil {
		return Textures{}, false, err
	}
	properties := textureProperties(profile.Properties)
	if len(properties) == 0 {
		return Textures{ProfileID: profile.ID, ProfileName: profile.Name, Textures: map[string]Texture{}}, false, nil
	}
	if len(properties) != 1 {
		return Textures{}, false, errors.New("profile contains duplicate textures properties")
	}
	property := properties[0]
	value, err := base64.StdEncoding.DecodeString(property.Value)
	if err != nil || len(value) == 0 || len(value) > maxTexturesSize {
		return Textures{}, false, errors.New("invalid textures property encoding")
	}
	if err := verifyPropertySignature([]byte(property.Value), property.Signature, publicKey); err != nil {
		return Textures{}, false, err
	}
	var textures Textures
	if err := json.Unmarshal(value, &textures); err != nil {
		return Textures{}, false, errors.New("invalid textures property JSON")
	}
	if err := validateTextures(textures); err != nil {
		return Textures{}, false, err
	}
	if !strings.EqualFold(textures.ProfileID, profile.ID) || textures.ProfileName != profile.Name {
		return Textures{}, false, errors.New("textures property does not match profile")
	}
	return textures, true, nil
}

func RewriteTextures(profile SignedProfile, textures Textures, privateKey *rsa.PrivateKey, now time.Time) (SignedProfile, error) {
	if err := validateProfile(profile); err != nil {
		return SignedProfile{}, err
	}
	if err := validatePrivateKey(privateKey); err != nil {
		return SignedProfile{}, err
	}
	properties := textureProperties(profile.Properties)
	if len(properties) > 1 {
		return SignedProfile{}, errors.New("profile contains duplicate textures properties")
	}
	textures.Timestamp = now.UnixMilli()
	if textures.ProfileID == "" {
		textures.ProfileID = profile.ID
	}
	if textures.ProfileName == "" {
		textures.ProfileName = profile.Name
	}
	if textures.Textures == nil {
		textures.Textures = map[string]Texture{}
	}
	if err := validateTextures(textures); err != nil {
		return SignedProfile{}, err
	}
	if !strings.EqualFold(textures.ProfileID, profile.ID) || textures.ProfileName != profile.Name {
		return SignedProfile{}, errors.New("textures property does not match profile")
	}
	value, err := json.Marshal(textures)
	if err != nil {
		return SignedProfile{}, fmt.Errorf("encode textures property: %w", err)
	}
	if len(value) > maxTexturesSize {
		return SignedProfile{}, errors.New("textures property exceeds size limit")
	}
	encodedValue := base64.StdEncoding.EncodeToString(value)
	// Yggdrasil requires SHA1withRSA for signed profile properties.
	digest := sha1.Sum([]byte(encodedValue))
	signature, err := rsa.SignPKCS1v15(nil, privateKey, crypto.SHA1, digest[:])
	if err != nil {
		return SignedProfile{}, fmt.Errorf("sign textures property: %w", err)
	}
	property := ProfileProperty{
		Name:      TexturesProperty,
		Value:     encodedValue,
		Signature: base64.StdEncoding.EncodeToString(signature),
	}
	updated := SignedProfile{ID: profile.ID, Name: profile.Name, Properties: append([]ProfileProperty(nil), profile.Properties...)}
	replaced := false
	for index := range updated.Properties {
		if updated.Properties[index].Name == TexturesProperty {
			updated.Properties[index] = property
			replaced = true
			break
		}
	}
	if !replaced {
		updated.Properties = append(updated.Properties, property)
	}
	return updated, nil
}

func verifyPropertySignature(value []byte, signature string, publicKey *rsa.PublicKey) error {
	if signature == "" {
		return errors.New("textures property is not signed")
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return errors.New("invalid textures property signature encoding")
	}
	digest := sha1.Sum(value)
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA1, digest[:], decoded); err != nil {
		return errors.New("textures property signature verification failed")
	}
	return nil
}

func validateTextures(textures Textures) error {
	if textures.Timestamp <= 0 {
		return errors.New("textures timestamp must be positive")
	}
	if !ValidProfileID(textures.ProfileID) {
		return errors.New("textures profile ID must be an unhyphenated UUID")
	}
	if !validProfileName(textures.ProfileName) {
		return errors.New("textures profile name is invalid")
	}
	for name, texture := range textures.Textures {
		parsed, err := url.Parse(texture.URL)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("texture %s has an invalid URL", name)
		}
		if model, ok := texture.Metadata["model"]; ok && model != "default" && model != "slim" {
			return fmt.Errorf("texture %s has an invalid model", name)
		}
	}
	return nil
}

func validateProfile(profile SignedProfile) error {
	if !ValidProfileID(profile.ID) {
		return errors.New("profile ID must be an unhyphenated UUID")
	}
	if !validProfileName(profile.Name) {
		return errors.New("profile name is invalid")
	}
	return nil
}

func ValidProfileID(id string) bool {
	if len(id) != 32 || !utf8.ValidString(id) {
		return false
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func validProfileName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

func textureProperties(properties []ProfileProperty) []ProfileProperty {
	var textures []ProfileProperty
	for _, property := range properties {
		if property.Name == TexturesProperty {
			textures = append(textures, property)
		}
	}
	return textures
}

func parsePEMKey(data []byte) (any, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("invalid signature key PEM")
	}
	switch block.Type {
	case "PUBLIC KEY":
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid RSA public key")
		}
		return key, nil
	case "RSA PUBLIC KEY":
		key, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid RSA public key")
		}
		return key, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid RSA private key")
		}
		return key, nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid RSA private key")
		}
		return key, nil
	default:
		return nil, errors.New("unsupported signature key PEM type")
	}
}

func validatePublicKey(key *rsa.PublicKey) (*rsa.PublicKey, error) {
	if key == nil || key.N == nil || key.N.BitLen() < 2048 || key.E < 2 {
		return nil, errors.New("invalid RSA public key")
	}
	return key, nil
}

func validatePrivateKey(key *rsa.PrivateKey) error {
	if key == nil {
		return errors.New("RSA private key is required")
	}
	if err := key.Validate(); err != nil || key.N.BitLen() < 2048 {
		return errors.New("invalid RSA private key")
	}
	return nil
}
