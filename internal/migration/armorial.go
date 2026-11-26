package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/corematch/scribe/internal/assets"
	"github.com/corematch/scribe/internal/protocol"
)

var (
	ErrInvalidCommit    = errors.New("invalid migration commit")
	ErrUnauthorized     = errors.New("migration service unauthorized")
	ErrForbidden        = errors.New("migration service forbidden")
	ErrCommitConflict   = errors.New("migration commit conflict")
	ErrCommitTooLarge   = errors.New("migration commit exceeds size limit")
	ErrResponseTooLarge = errors.New("migration response exceeds size limit")
	ErrUpstream         = errors.New("migration upstream unavailable")
	ErrInvalidResponse  = errors.New("invalid migration upstream response")
)

type APIError struct {
	StatusCode int
	Code       string
	Err        error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("migration upstream returned %d (%s)", e.StatusCode, e.Code)
}

func (e *APIError) Unwrap() error { return e.Err }

type ClientOptions struct {
	Timeout         time.Duration
	MaxResponseBody int64
	MaxRequestBody  int64
}

type Client struct {
	baseURL         *url.URL
	serviceToken    string
	client          *http.Client
	maxResponseBody int64
	maxRequestBody  int64
}

type CommitRequest struct {
	Staged               StagedMigration
	Identifier           string
	Password             string
	LegacyUserID         string
	LegacyUserProperties map[string]string
}

type CommitMetadata struct {
	MigrationID          string            `json:"migration_id"`
	Identifier           string            `json:"identifier"`
	Password             string            `json:"password"`
	Username             string            `json:"username"`
	Email                string            `json:"email,omitempty"`
	LegacyUserID         string            `json:"legacy_user_id,omitempty"`
	LegacyUserProperties map[string]string `json:"legacy_user_properties,omitempty"`
	ProfileUUID          string            `json:"profile_uuid"`
	ProfileName          string            `json:"profile_name"`
	ProfileModel         string            `json:"profile_model"`
	SkinSHA256           string            `json:"skin_sha256,omitempty"`
	CapeSHA256           string            `json:"cape_sha256,omitempty"`
	SkinSync             bool              `json:"skin_sync"`
}

type CommitResult struct {
	MigrationID     string         `json:"migration_id"`
	Status          string         `json:"status"`
	CoreUID         string         `json:"core_uid"`
	AccountID       int64          `json:"account_id"`
	ProfileID       string         `json:"profile_id"`
	CreatedCoreUser bool           `json:"created_core_user"`
	ReusedCoreUser  bool           `json:"reused_core_user"`
	Session         Session        `json:"session"`
	User            *protocol.User `json:"user"`
	Assets          AssetResults   `json:"assets"`
	CommittedAt     time.Time      `json:"committed_at"`
}

type commitResultWire struct {
	CommitResult
	AccountID *int64 `json:"account_id"`
}

type Session struct {
	AccessToken       string             `json:"accessToken"`
	ClientToken       string             `json:"clientToken"`
	SelectedProfile   protocol.Profile   `json:"selectedProfile"`
	AvailableProfiles []protocol.Profile `json:"availableProfiles,omitempty"`
}

type AssetResults struct {
	Skin AssetResult `json:"skin"`
	Cape AssetResult `json:"cape"`
}

type AssetResult struct {
	Status string `json:"status"`
	SHA256 string `json:"sha256,omitempty"`
}

func NewClient(rawBaseURL, serviceToken string, options ClientOptions) (*Client, error) {
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Hostname() == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" || (baseURL.Path != "" && baseURL.Path != "/") {
		return nil, errors.New("invalid migration upstream base URL")
	}
	if serviceToken == "" || strings.TrimSpace(serviceToken) != serviceToken || strings.ContainsFunc(serviceToken, func(char rune) bool {
		return unicode.IsControl(char) || unicode.IsSpace(char)
	}) {
		return nil, errors.New("invalid migration service token")
	}
	if options.Timeout < 0 || options.MaxResponseBody < 0 || options.MaxRequestBody < 0 {
		return nil, errors.New("migration client limits must not be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 60 * time.Second
	}
	if options.MaxResponseBody == 0 {
		options.MaxResponseBody = 1 << 20
	}
	if options.MaxRequestBody == 0 {
		options.MaxRequestBody = 16 << 20
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: options.Timeout,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       30 * time.Second,
	}
	return &Client{
		baseURL:      baseURL,
		serviceToken: serviceToken,
		client: &http.Client{
			Transport: transport,
			Timeout:   options.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxResponseBody: options.MaxResponseBody,
		maxRequestBody:  options.MaxRequestBody,
	}, nil
}

func (c *Client) Validate(request CommitRequest) error {
	_, _, _, err := c.prepare(request)
	return err
}

func (c *Client) prepare(request CommitRequest) (CommitMetadata, []byte, string, error) {
	metadata, err := buildCommitMetadata(request)
	if err != nil {
		return CommitMetadata{}, nil, "", err
	}
	body, contentType, err := encodeCommit(metadata, request.Staged)
	if err != nil {
		return CommitMetadata{}, nil, "", ErrInvalidCommit
	}
	if int64(len(body)) > c.maxRequestBody {
		return CommitMetadata{}, nil, "", ErrCommitTooLarge
	}
	return metadata, body, contentType, nil
}

func (c *Client) Commit(ctx context.Context, request CommitRequest) (CommitResult, error) {
	metadata, body, contentType, err := c.prepare(request)
	if err != nil {
		return CommitResult{}, err
	}
	target := *c.baseURL
	target.Path = "/internal/migrations/commit"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return CommitResult{}, ErrInvalidCommit
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.serviceToken)
	httpRequest.Header.Set("Idempotency-Key", metadata.MigrationID)
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.ContentLength = int64(len(body))
	response, err := c.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return CommitResult{}, ctx.Err()
		}
		return CommitResult{}, ErrUpstream
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return CommitResult{}, ctx.Err()
		}
		return CommitResult{}, ErrUpstream
	}
	if int64(len(responseBody)) > c.maxResponseBody {
		return CommitResult{}, ErrResponseTooLarge
	}
	if response.StatusCode != http.StatusOK {
		return CommitResult{}, migrationAPIError(response.StatusCode, responseBody)
	}
	var envelope struct {
		Success bool             `json:"success"`
		Data    commitResultWire `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil || !envelope.Success || envelope.Data.AccountID == nil {
		return CommitResult{}, ErrInvalidResponse
	}
	result := envelope.Data.CommitResult
	result.AccountID = *envelope.Data.AccountID
	if err := validateCommitResult(result, request.Staged); err != nil {
		return CommitResult{}, err
	}
	return result, nil
}

func validateCommitCredentials(profile protocol.Profile, password string) error {
	if !protocol.ValidProfileID(profile.ID) || !validCommitName(profile.Name) ||
		!utf8.ValidString(password) || utf8.RuneCountInString(password) < 6 {
		return ErrInvalidCommit
	}
	return nil
}

func buildCommitMetadata(request CommitRequest) (CommitMetadata, error) {
	staged := request.Staged
	if !validMigrationID(staged.MigrationID) {
		return CommitMetadata{}, ErrInvalidCommit
	}
	if err := validateCommitCredentials(staged.Profile, request.Password); err != nil {
		return CommitMetadata{}, err
	}
	identifier, err := protocol.NormalizeIdentifier(request.Identifier)
	if err != nil {
		return CommitMetadata{}, ErrInvalidCommit
	}
	identifierDigest := sha256.Sum256([]byte(identifier))
	identifierHash := hex.EncodeToString(identifierDigest[:])
	canonicalHash, err := canonicalSHA256(staged.IdentifierHash)
	if err != nil || !strings.EqualFold(canonicalHash, identifierHash) {
		return CommitMetadata{}, ErrInvalidCommit
	}
	digest, err := CommitDigest(staged.snapshot(), canonicalHash)
	if err != nil || !strings.EqualFold(digest, staged.CommitDigest) {
		return CommitMetadata{}, ErrInvalidCommit
	}
	if err := validateStagedAsset(staged.Skin); err != nil {
		return CommitMetadata{}, err
	}
	if err := validateStagedAsset(staged.Cape); err != nil {
		return CommitMetadata{}, err
	}
	model := "default"
	if staged.Skin != nil {
		if skinModel, ok := staged.Skin.Metadata["model"]; ok {
			model = skinModel
		}
	}
	if model != "default" && model != "slim" {
		return CommitMetadata{}, ErrInvalidCommit
	}
	metadata := CommitMetadata{
		MigrationID:          staged.MigrationID,
		Identifier:           identifier,
		Password:             request.Password,
		Username:             staged.Profile.Name,
		LegacyUserID:         request.LegacyUserID,
		LegacyUserProperties: request.LegacyUserProperties,
		ProfileUUID:          staged.Profile.ID,
		ProfileName:          staged.Profile.Name,
		ProfileModel:         model,
		SkinSync:             false,
	}
	if address, err := mail.ParseAddress(identifier); err == nil && address.Address == identifier {
		metadata.Email = identifier
	}
	if staged.Skin != nil {
		metadata.SkinSHA256 = staged.Skin.PixelSHA256
	}
	if staged.Cape != nil {
		metadata.CapeSHA256 = staged.Cape.PixelSHA256
	}
	return metadata, nil
}

func encodeCommit(metadata CommitMetadata, staged StagedMigration) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, "", err
	}
	metadataPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": []string{`form-data; name="metadata"`},
		"Content-Type":        []string{"application/json"},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err := metadataPart.Write(metadataJSON); err != nil {
		return nil, "", err
	}
	for _, part := range []struct {
		name string
		data []byte
	}{{name: "skin", data: stagedPNG(staged.Skin)}, {name: "cape", data: stagedPNG(staged.Cape)}} {
		if part.data == nil {
			continue
		}
		assetPart, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": []string{fmt.Sprintf(`form-data; name="%s"; filename="%s.png"`, part.name, part.name)},
			"Content-Type":        []string{"image/png"},
		})
		if err != nil {
			return nil, "", err
		}
		if _, err := assetPart.Write(part.data); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func validateStagedAsset(asset *StagedAsset) error {
	if asset == nil {
		return nil
	}
	limits := assets.DefaultLimits()
	if _, err := canonicalSHA256(asset.PixelSHA256); err != nil || int64(len(asset.PNG)) > limits.MaxEncodedBytes ||
		asset.Width <= 0 || asset.Height <= 0 || asset.Width > limits.MaxWidth || asset.Height > limits.MaxHeight ||
		asset.Width > limits.MaxPixels/asset.Height {
		return ErrInvalidCommit
	}
	normalized, err := assets.NormalizePNG(asset.PNG, limits)
	if err != nil || normalized.Width != asset.Width || normalized.Height != asset.Height ||
		!strings.EqualFold(normalized.PixelSHA256, asset.PixelSHA256) || !bytes.Equal(normalized.PNG, asset.PNG) {
		return ErrInvalidCommit
	}
	return nil
}

func validateCommitResult(result CommitResult, staged StagedMigration) error {
	if !strings.EqualFold(result.MigrationID, staged.MigrationID) || result.Status != "committed" ||
		!protocol.ValidProfileID(result.ProfileID) || !strings.EqualFold(result.ProfileID, staged.Profile.ID) ||
		result.CoreUID == "" || result.CreatedCoreUser == result.ReusedCoreUser || result.CommittedAt.IsZero() {
		return ErrInvalidResponse
	}
	session := result.Session
	if session.AccessToken == "" || session.ClientToken == "" ||
		!strings.EqualFold(session.SelectedProfile.ID, staged.Profile.ID) || session.SelectedProfile.Name != staged.Profile.Name {
		return ErrInvalidResponse
	}
	if err := validateMigrationUser(result.User, result.ProfileID); err != nil {
		return ErrInvalidResponse
	}
	if err := validateAssetResult(result.Assets.Skin, staged.Skin); err != nil {
		return err
	}
	return validateAssetResult(result.Assets.Cape, staged.Cape)
}

func validateMigrationUser(user *protocol.User, profileID string) error {
	if user == nil || !protocol.ValidProfileID(user.ID) || !strings.EqualFold(user.ID, profileID) {
		return ErrInvalidResponse
	}
	for _, property := range user.Properties {
		name, hasName := property["name"]
		_, hasValue := property["value"]
		if len(property) != 2 || !hasName || !hasValue || name == "" {
			return ErrInvalidResponse
		}
	}
	return nil
}

func validateAssetResult(result AssetResult, asset *StagedAsset) error {
	if asset == nil {
		if result.Status != "absent" || result.SHA256 != "" {
			return ErrInvalidResponse
		}
		return nil
	}
	if result.Status != "uploaded" && result.Status != "unchanged" {
		return ErrInvalidResponse
	}
	if !strings.EqualFold(result.SHA256, asset.PixelSHA256) {
		return ErrInvalidResponse
	}
	return nil
}

func migrationAPIError(status int, body []byte) error {
	var envelope struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	code := safeErrorCode(envelope.Code)
	var sentinel error
	switch status {
	case http.StatusBadRequest:
		sentinel = ErrInvalidCommit
	case http.StatusUnauthorized:
		sentinel = ErrUnauthorized
	case http.StatusForbidden:
		sentinel = ErrForbidden
	case http.StatusConflict:
		sentinel = ErrCommitConflict
	case http.StatusRequestEntityTooLarge:
		sentinel = ErrCommitTooLarge
	default:
		sentinel = ErrUpstream
	}
	return &APIError{StatusCode: status, Code: code, Err: sentinel}
}

func safeErrorCode(code string) string {
	if code == "" || len(code) > 64 {
		return "migration_commit_failed"
	}
	for _, char := range code {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '_' {
			return "migration_commit_failed"
		}
	}
	return code
}

func validCommitName(name string) bool {
	return utf8.RuneCountInString(name) >= 3 && strings.TrimSpace(name) == name && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

func stagedPNG(asset *StagedAsset) []byte {
	if asset == nil {
		return nil
	}
	return asset.PNG
}
