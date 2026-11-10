package teams

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Bot Framework constants (verified, see package doc).
const (
	OpenIDMetadataURL  = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	BotFrameworkIssuer = "https://api.botframework.com"
	ClockSkew          = 5 * time.Minute
)

// ErrUnauthorized is returned for invalid inbound tokens.
var ErrUnauthorized = errors.New("teams: unauthorized")

type jwk struct {
	Kid          string   `json:"kid"`
	Kty          string   `json:"kty"`
	N            string   `json:"n"`
	E            string   `json:"e"`
	Endorsements []string `json:"endorsements"`
}

type key struct {
	pub          *rsa.PublicKey
	endorsements []string
}

// Validator validates inbound Bot Connector JWTs.
type Validator struct {
	AppID       string
	MetadataURL string
	HTTP        *http.Client
	Now         func() time.Time

	mu      sync.Mutex
	keys    map[string]key
	fetched time.Time
}

// NewValidator builds a validator for the bot's app ID.
func NewValidator(appID string, hc *http.Client) *Validator {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Validator{AppID: appID, MetadataURL: OpenIDMetadataURL, HTTP: hc, Now: time.Now}
}

func (v *Validator) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (v *Validator) refresh(ctx context.Context, force bool) error {
	v.mu.Lock()
	fresh := v.keys != nil && v.Now().Sub(v.fetched) < 24*time.Hour
	v.mu.Unlock()
	if fresh && !force {
		return nil
	}
	var meta struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, v.MetadataURL, &meta); err != nil {
		return fmt.Errorf("openid metadata: %w", err)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.getJSON(ctx, meta.JWKSURI, &set); err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	keys := map[string]key{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = key{pub: &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}, endorsements: k.Endorsements}
	}
	v.mu.Lock()
	v.keys, v.fetched = keys, v.Now()
	v.mu.Unlock()
	return nil
}

// Validate checks the Authorization header for an activity with the given
// serviceUrl and channelId.
func (v *Validator) Validate(ctx context.Context, authHeader, serviceURL, channelID string) error {
	raw, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok || raw == "" {
		return fmt.Errorf("%w: missing bearer token", ErrUnauthorized)
	}
	if err := v.refresh(ctx, false); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var used key
	keyfunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		v.mu.Lock()
		k, ok := v.keys[kid]
		v.mu.Unlock()
		if !ok {
			// Key rotation: refetch once.
			if err := v.refresh(ctx, true); err != nil {
				return nil, err
			}
			v.mu.Lock()
			k, ok = v.keys[kid]
			v.mu.Unlock()
			if !ok {
				return nil, fmt.Errorf("unknown signing key %q", kid)
			}
		}
		used = k
		return k.pub, nil
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(BotFrameworkIssuer),
		jwt.WithAudience(v.AppID),
		jwt.WithLeeway(ClockSkew),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.Now),
	)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if su, _ := claims["serviceUrl"].(string); su == "" || !strings.EqualFold(strings.TrimRight(su, "/"), strings.TrimRight(serviceURL, "/")) {
		return fmt.Errorf("%w: serviceUrl claim does not match activity", ErrUnauthorized)
	}
	if len(used.endorsements) > 0 && !slices.Contains(used.endorsements, channelID) {
		return fmt.Errorf("%w: signing key not endorsed for channel %q", ErrUnauthorized, channelID)
	}
	return nil
}
