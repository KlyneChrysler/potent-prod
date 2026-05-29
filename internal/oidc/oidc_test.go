package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// keyMaterial bundles a fresh RSA keypair, the matching JWKS document
// served as bytes, the kid both halves agree on, and a signer that
// produces JWTs already populated with required claims.
type keyMaterial struct {
	private *rsa.PrivateKey
	jwks    []byte
	kid     string
}

func newKey(t *testing.T) keyMaterial {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	kid := "test-kid-1"
	n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(bigEndian(priv.E))
	doc := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"alg": "RS256",
				"n":   n,
				"e":   e,
			},
		},
	}
	body, _ := json.Marshal(doc)
	return keyMaterial{private: priv, jwks: body, kid: kid}
}

// bigEndian encodes an int as a minimal big-endian byte slice (RSA's E).
func bigEndian(x int) []byte {
	out := []byte{byte(x >> 16), byte(x >> 8), byte(x)}
	// strip leading zeros
	for len(out) > 1 && out[0] == 0 {
		out = out[1:]
	}
	return out
}

func (k keyMaterial) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = k.kid
	signed, err := tok.SignedString(k.private)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// jwksServer serves the supplied document at /.well-known/jwks.json and
// counts requests so tests can verify refresh behavior.
func jwksServer(t *testing.T, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestVerifier_HappyPath(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, err := NewVerifier(Config{
		JWKSURL:  srv.URL,
		Issuer:   "https://example.test",
		Audience: "potent",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	tok := k.sign(t, jwt.MapClaims{
		"iss": "https://example.test",
		"aud": "potent",
		"sub": "user-42",
		"exp": time.Now().Add(time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})
	caller, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if caller != "user-42" {
		t.Errorf("caller = %q, want user-42", caller)
	}
}

func TestVerifier_RejectsWrongIssuer(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL, Issuer: "https://expected.test"})
	_ = v.Refresh(context.Background())
	tok := k.sign(t, jwt.MapClaims{
		"iss": "https://imposter.test",
		"sub": "user-42",
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	if _, err := v.Verify(tok); err == nil {
		t.Errorf("expected error for wrong issuer")
	}
}

func TestVerifier_RejectsWrongAudience(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL, Audience: "potent"})
	_ = v.Refresh(context.Background())
	tok := k.sign(t, jwt.MapClaims{
		"aud": "other-service",
		"sub": "user-42",
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	if _, err := v.Verify(tok); err == nil {
		t.Errorf("expected error for wrong audience")
	}
}

func TestVerifier_RejectsExpired(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL})
	_ = v.Refresh(context.Background())
	tok := k.sign(t, jwt.MapClaims{
		"sub": "user-42",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	if _, err := v.Verify(tok); err == nil {
		t.Errorf("expected error for expired token")
	}
}

func TestVerifier_RejectsUnknownKid(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL})
	_ = v.Refresh(context.Background())

	// Sign with a fresh keypair whose kid is not in the JWKS.
	otherKey := newKey(t)
	otherKey.kid = "wrong-kid"
	tok := otherKey.sign(t, jwt.MapClaims{
		"sub": "user-42",
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	if _, err := v.Verify(tok); err == nil {
		t.Errorf("expected error for unknown kid")
	}
}

func TestVerifier_RejectsNoneAlg(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL})
	_ = v.Refresh(context.Background())

	// A literal none-alg JWT: header.payload.<empty>
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"sneaky"}`))
	tok := hdr + "." + body + "."
	if _, err := v.Verify(tok); err == nil {
		t.Errorf("verifier accepted none-alg token")
	}
}

func TestVerifier_CustomCallerClaim(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL, CallerClaim: "email"})
	_ = v.Refresh(context.Background())
	tok := k.sign(t, jwt.MapClaims{
		"sub":   "user-42",
		"email": "alice@example.com",
		"exp":   time.Now().Add(time.Minute).Unix(),
	})
	caller, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if caller != "alice@example.com" {
		t.Errorf("caller = %q, want alice@example.com", caller)
	}
}

func TestNewVerifier_RejectsEmptyJWKSURL(t *testing.T) {
	if _, err := NewVerifier(Config{}); err == nil {
		t.Errorf("expected error for empty JWKSURL")
	}
}

func TestRefresh_HandlesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	v, _ := NewVerifier(Config{JWKSURL: srv.URL})
	if err := v.Refresh(context.Background()); err == nil {
		t.Errorf("expected error on 503")
	}
}

func TestRefresh_HandlesMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	v, _ := NewVerifier(Config{JWKSURL: srv.URL})
	if err := v.Refresh(context.Background()); err == nil {
		t.Errorf("expected error on bad json")
	}
}

func TestRun_RefreshesPeriodically(t *testing.T) {
	k := newKey(t)
	srv, count := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL, RefreshInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = v.Run(ctx)
	if count.Load() < 2 {
		t.Errorf("expected >= 2 refresh fetches, got %d", count.Load())
	}
}

func TestParseJWKS_AcceptsECP256Key(t *testing.T) {
	// A well-formed EC P-256 JWKS sample from rfc7517 appendix A.1.
	body := []byte(`{"keys":[{
		"kty":"EC","kid":"ec-1","crv":"P-256",
		"x":"f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU",
		"y":"x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"
	}]}`)
	keys, err := parseJWKS(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 ec key, got %d", len(keys))
	}
}

func TestParseJWKS_RejectsBadRSAExponent(t *testing.T) {
	body := []byte(`{"keys":[{"kty":"RSA","kid":"bad","n":"AQAB","e":"!!!"}]}`)
	if _, err := parseJWKS(body); err == nil {
		t.Errorf("expected error for malformed RSA exponent")
	}
}

func TestParseJWKS_RejectsUnsupportedCurve(t *testing.T) {
	body := []byte(`{"keys":[{"kty":"EC","kid":"x","crv":"P-999","x":"AA","y":"AA"}]}`)
	if _, err := parseJWKS(body); err == nil {
		t.Errorf("expected error for unsupported curve")
	}
}

func TestParseJWKS_SkipsUnsupportedKty(t *testing.T) {
	body := []byte(`{"keys":[{"kty":"oct","kid":"sym"}]}`)
	keys, err := parseJWKS(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("oct key should be skipped, got %d keys", len(keys))
	}
}

func TestVerify_RejectsNonStringClaim(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL, CallerClaim: "id"})
	_ = v.Refresh(context.Background())
	tok := k.sign(t, jwt.MapClaims{
		"id":  42, // numeric, not a string
		"sub": "fallback",
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	if _, err := v.Verify(tok); err == nil {
		t.Errorf("expected error for non-string caller claim")
	}
}

func TestVerify_RejectsAbsentClaim(t *testing.T) {
	k := newKey(t)
	srv, _ := jwksServer(t, k.jwks)
	v, _ := NewVerifier(Config{JWKSURL: srv.URL, CallerClaim: "email"})
	_ = v.Refresh(context.Background())
	tok := k.sign(t, jwt.MapClaims{
		"sub": "user-42",
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	if _, err := v.Verify(tok); err == nil || !strings.Contains(fmt.Sprint(err), "email") {
		t.Errorf("expected error mentioning email claim, got %v", err)
	}
}
