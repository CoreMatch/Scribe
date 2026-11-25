package profile

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/corematch/scribe/internal/protocol"
)

var (
	ErrProfileNotFound  = errors.New("signed profile not found")
	ErrUpstream         = errors.New("profile upstream unavailable")
	ErrResponseTooLarge = errors.New("profile response exceeds size limit")
	ErrInvalidMetadata  = errors.New("invalid profile verification metadata")
)

type Options struct {
	Timeout         time.Duration
	MaxResponseBody int64
}

type Client struct {
	baseURL         *url.URL
	client          *http.Client
	maxResponseBody int64
}

func New(rawBaseURL string, options Options) (*Client, error) {
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Hostname() == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" || (baseURL.Path != "" && baseURL.Path != "/") {
		return nil, errors.New("invalid profile upstream base URL")
	}
	if options.Timeout < 0 || options.MaxResponseBody < 0 {
		return nil, errors.New("profile client limits must not be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 15 * time.Second
	}
	if options.MaxResponseBody == 0 {
		options.MaxResponseBody = 1 << 20
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
		baseURL: baseURL,
		client: &http.Client{
			Transport: transport,
			Timeout:   options.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxResponseBody: options.MaxResponseBody,
	}, nil
}

func (c *Client) FetchVerificationKey(ctx context.Context) (*rsa.PublicKey, error) {
	body, status, err := c.get(ctx, "/", "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, ErrUpstream
	}
	var metadata struct {
		SignaturePublicKey string `json:"signaturePublickey"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil || metadata.SignaturePublicKey == "" {
		return nil, ErrInvalidMetadata
	}
	key, err := protocol.ParsePublicKey([]byte(metadata.SignaturePublicKey))
	if err != nil {
		return nil, ErrInvalidMetadata
	}
	return key, nil
}

func (c *Client) FetchProfile(ctx context.Context, profileID string, publicKey *rsa.PublicKey) (protocol.SignedProfile, protocol.Textures, bool, error) {
	if !protocol.ValidProfileID(profileID) {
		return protocol.SignedProfile{}, protocol.Textures{}, false, errors.New("invalid profile ID")
	}
	body, status, err := c.get(ctx, "/sessionserver/session/minecraft/profile/"+profileID, "unsigned=false")
	if err != nil {
		return protocol.SignedProfile{}, protocol.Textures{}, false, err
	}
	if status == http.StatusNoContent {
		return protocol.SignedProfile{}, protocol.Textures{}, false, ErrProfileNotFound
	}
	if status != http.StatusOK {
		return protocol.SignedProfile{}, protocol.Textures{}, false, ErrUpstream
	}
	profile, err := protocol.DecodeSignedProfile(body)
	if err != nil {
		return protocol.SignedProfile{}, protocol.Textures{}, false, err
	}
	if !strings.EqualFold(profile.ID, profileID) {
		return protocol.SignedProfile{}, protocol.Textures{}, false, errors.New("profile response ID does not match request")
	}
	textures, present, err := protocol.VerifiedTextures(profile, publicKey)
	if err != nil {
		return protocol.SignedProfile{}, protocol.Textures{}, false, err
	}
	return profile, textures, present, nil
}

func (c *Client) get(ctx context.Context, path, query string) ([]byte, int, error) {
	target := *c.baseURL
	target.Path = path
	target.RawQuery = query
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, ErrUpstream
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, ErrUpstream
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, ErrUpstream
	}
	if int64(len(body)) > c.maxResponseBody {
		return nil, 0, ErrResponseTooLarge
	}
	return body, response.StatusCode, nil
}
