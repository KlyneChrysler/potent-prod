// Package oidc validates inbound bearer tokens against an OpenID Connect
// identity provider's JWKS endpoint. It is deliberately small: just enough
// to integrate with Okta / Auth0 / Azure AD / Keycloak / Google for the
// common case where the IdP signs short-lived JWTs and the proxy needs to
// extract a caller-id from a standard claim.
//
// Cryptography lives in github.com/golang-jwt/jwt/v5; we add a JWKS
// fetcher + key cache + claim validator on top so the auth middleware
// can call Verify(token) -> caller-id and let the rest of the codebase
// stay unchanged.
package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config holds everything Verifier needs at construction time. JWKSURL
// is the only required field; everything else has sensible defaults.
type Config struct {
	// JWKSURL is the IdP's published JSON Web Key Set endpoint.
	// Typically /.well-known/jwks.json under the issuer.
	JWKSURL string

	// Issuer, when set, requires that the JWT's `iss` claim equals this
	// value. Strongly recommended in production.
	Issuer string

	// Audience, when set, requires the JWT's `aud` claim contain this
	// value (the spec permits aud to be a string or string array).
	Audience string

	// CallerClaim is the JWT claim the auth middleware extracts as the
	// caller-id (for per-tool ACLs and audit logs). Defaults to "sub"
	// which every spec-conformant IdP populates.
	CallerClaim string

	// RefreshInterval bounds how often the JWKS cache is re-fetched in
	// the background. Defaults to one hour; lowering it speeds key
	// rotation, raising it reduces upstream load.
	RefreshInterval time.Duration

	// HTTPClient overrides the default client used to fetch JWKS.
	// Useful for tests, proxies, and TLS pinning.
	HTTPClient *http.Client

	// Logger receives JWKS refresh successes and failures.
	Logger *slog.Logger
}

// DefaultRefreshInterval is the JWKS-cache refresh cadence when Config
// does not set one.
const DefaultRefreshInterval = time.Hour

// Verifier validates JWTs against a periodically-refreshed JWKS cache.
// Construct with NewVerifier and call Run to start the background
// refresher; close ctx to stop it.
type Verifier struct {
	cfg    Config
	client *http.Client
	parser *jwt.Parser

	mu   sync.RWMutex
	keys map[string]interface{} // kid -> *rsa.PublicKey or *ecdsa.PublicKey
}

// NewVerifier constructs a Verifier. The keys map is populated on first
// Run cycle so callers must either Run before Verify or accept that the
// first Verify will fail until a refresh succeeds.
func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.JWKSURL == "" {
		return nil, errors.New("oidc: JWKSURL is required")
	}
	if cfg.CallerClaim == "" {
		cfg.CallerClaim = "sub"
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultRefreshInterval
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}),
		jwt.WithLeeway(30 * time.Second),
	}
	if cfg.Issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(cfg.Audience))
	}
	return &Verifier{
		cfg:    cfg,
		client: cfg.HTTPClient,
		parser: jwt.NewParser(parserOpts...),
		keys:   map[string]interface{}{},
	}, nil
}

// Run drives the background JWKS refresh loop until ctx is cancelled. It
// performs an immediate refresh so the first Verify after Run returns
// has keys available. Returns when ctx is done.
func (v *Verifier) Run(ctx context.Context) error {
	if err := v.Refresh(ctx); err != nil {
		v.cfg.Logger.Warn("oidc jwks initial refresh failed", "err", err)
		// keep running; we may recover on the next tick
	}
	ticker := time.NewTicker(v.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := v.Refresh(ctx); err != nil {
				v.cfg.Logger.Warn("oidc jwks refresh failed", "err", err)
			}
		}
	}
}

// Refresh fetches the JWKS document and swaps the key cache. Exported so
// tests can drive a single refresh without spinning up the loop.
func (v *Verifier) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return fmt.Errorf("build jwks request: %w", err)
	}
	resp, err := v.client.Do(req) // #nosec G107 -- JWKSURL is operator-configured
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read jwks: %w", err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return fmt.Errorf("parse jwks: %w", err)
	}
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	v.cfg.Logger.Info("oidc jwks refreshed", "keys", len(keys), "url", v.cfg.JWKSURL)
	return nil
}

// Verify parses, validates, and returns the caller-id extracted from a
// JWT. The bearer prefix has already been stripped by the caller.
func (v *Verifier) Verify(token string) (string, error) {
	parsed, err := v.parser.Parse(token, func(t *jwt.Token) (interface{}, error) {
		kid, _ := t.Header["kid"].(string)
		v.mu.RLock()
		key, ok := v.keys[kid]
		v.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("no key for kid %q", kid)
		}
		return key, nil
	})
	if err != nil {
		return "", err
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("oidc: token claims not a json object")
	}
	rawCaller, ok := claims[v.cfg.CallerClaim]
	if !ok {
		return "", fmt.Errorf("oidc: claim %q absent", v.cfg.CallerClaim)
	}
	caller, ok := rawCaller.(string)
	if !ok || caller == "" {
		return "", fmt.Errorf("oidc: claim %q is not a non-empty string", v.cfg.CallerClaim)
	}
	return caller, nil
}

// parseJWKS reads a JSON Web Key Set document and returns the public
// keys indexed by kid. Only RSA and EC keys are supported (the
// algorithms in our parser allowlist).
func parseJWKS(body []byte) (map[string]interface{}, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch strings.ToUpper(k.Kty) {
		case "RSA":
			pub, err := rsaPublicKey(k.N, k.E)
			if err != nil {
				return nil, fmt.Errorf("rsa key %q: %w", k.Kid, err)
			}
			out[k.Kid] = pub
		case "EC":
			pub, err := ecPublicKey(k.Crv, k.X, k.Y)
			if err != nil {
				return nil, fmt.Errorf("ec key %q: %w", k.Kid, err)
			}
			out[k.Kid] = pub
		default:
			// silently skip unsupported types so JWKS additions don't break us
		}
	}
	return out, nil
}

func rsaPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("n: %w", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("e: %w", err)
	}
	var eInt int
	for _, b := range e {
		eInt = eInt<<8 + int(b)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: eInt}, nil
}

func ecPublicKey(crv, xB64, yB64 string) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch strings.ToUpper(crv) {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported curve %q", crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(xB64)
	if err != nil {
		return nil, fmt.Errorf("x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(yB64)
	if err != nil {
		return nil, fmt.Errorf("y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
}
