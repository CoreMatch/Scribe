package profile

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/corematch/scribe/internal/protocol"
)

const testProfileID = "12345678123412341234567812345678"

func TestFetchVerificationKeyAndSignedProfile(t *testing.T) {
	key := testRSAKey(t)
	profile := protocol.SignedProfile{ID: testProfileID, Name: "LegacyPlayer"}
	textures := protocol.Textures{Textures: map[string]protocol.Texture{
		"SKIN": {URL: "https://textures.example.com/skin.png", Metadata: map[string]string{"model": "slim"}},
	}}
	signed, err := protocol.RewriteTextures(profile, textures, key, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	profileBody, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_ = json.NewEncoder(w).Encode(map[string]string{"signaturePublickey": string(publicPEM)})
		case "/sessionserver/session/minecraft/profile/" + testProfileID:
			if r.URL.Query().Get("unsigned") != "false" {
				t.Fatalf("unsigned query = %q", r.URL.Query().Get("unsigned"))
			}
			_, _ = w.Write(profileBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := localTestClient(t, server, 1<<20)

	publicKey, err := client.FetchVerificationKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fetched, verified, present, err := client.FetchProfile(context.Background(), testProfileID, publicKey)
	if err != nil || !present || fetched.ID != testProfileID || verified.Textures["SKIN"].Metadata["model"] != "slim" {
		t.Fatalf("FetchProfile() = %+v, %+v, %v, %v", fetched, verified, present, err)
	}
}

func TestFetchProfileRejectsUnknownAndFailedUpstreams(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{name: "missing", status: http.StatusNoContent, want: ErrProfileNotFound},
		{name: "upstream failure", status: http.StatusBadGateway, want: ErrUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			client := localTestClient(t, server, 1<<20)
			if _, _, _, err := client.FetchProfile(context.Background(), testProfileID, &testRSAKey(t).PublicKey); !errors.Is(err, tt.want) {
				t.Fatalf("FetchProfile() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestFetchProfileRejectsOversizedAndTamperedResponses(t *testing.T) {
	key := testRSAKey(t)
	profile := protocol.SignedProfile{ID: testProfileID, Name: "LegacyPlayer"}
	textures := protocol.Textures{Textures: map[string]protocol.Texture{"SKIN": {URL: "https://textures.example.com/skin.png"}}}
	signed, err := protocol.RewriteTextures(profile, textures, key, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	signed.Properties[0].Value = base64JSON(`{"timestamp":1700000000000,"profileId":"12345678123412341234567812345678","profileName":"LegacyPlayer","textures":{}}`)
	body, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := localTestClient(t, server, 1<<20)
	if _, _, _, err := client.FetchProfile(context.Background(), testProfileID, &key.PublicKey); err == nil {
		t.Fatal("FetchProfile() accepted a tampered signature")
	}
	client = localTestClient(t, server, int64(len(body)-1))
	if _, _, _, err := client.FetchProfile(context.Background(), testProfileID, &key.PublicKey); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestFetchVerificationKeyRejectsInvalidMetadata(t *testing.T) {
	for _, body := range []string{`{}`, `{"signaturePublickey":"not PEM"}`, `not-json`} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		client := localTestClient(t, server, 1<<20)
		if _, err := client.FetchVerificationKey(context.Background()); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("FetchVerificationKey() error = %v for %q", err, body)
		}
		server.Close()
	}
}

func TestClientRejectsInvalidBaseURLsAndDoesNotFollowRedirects(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/prefix", "relative"} {
		if _, err := New(raw, Options{}); err == nil {
			t.Fatalf("New(%q) unexpectedly succeeded", raw)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, `{"signaturePublickey":"invalid"}`)
	}))
	defer server.Close()
	client := localTestClient(t, server, 1<<20)
	if _, err := client.FetchVerificationKey(context.Background()); !errors.Is(err, ErrUpstream) {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestClientRejectsInvalidOptions(t *testing.T) {
	for _, options := range []Options{{Timeout: -1}, {MaxResponseBody: -1}} {
		if _, err := New("https://example.com", options); err == nil {
			t.Fatalf("New(%+v) unexpectedly succeeded", options)
		}
	}
}

func localTestClient(t *testing.T, server *httptest.Server, maxBytes int64) *Client {
	t.Helper()
	client, err := New(server.URL, Options{Timeout: time.Second, MaxResponseBody: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	transport := client.client.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return client
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func base64JSON(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}
