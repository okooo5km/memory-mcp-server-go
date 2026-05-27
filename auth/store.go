// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"errors"
	"sync"
	"time"
)

var (
	errInvalidRefreshToken   = errors.New("invalid or expired refresh token")
	errRefreshClientMismatch = errors.New("client_id mismatch")
)

// OAuthClient represents a dynamically registered OAuth client (RFC 7591).
type OAuthClient struct {
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	ClientName    string    `json:"client_name,omitempty"`
	RedirectURIs  []string  `json:"redirect_uris"`
	GrantTypes    []string  `json:"grant_types"`
	ResponseTypes []string  `json:"response_types"`
	CreatedAt     time.Time `json:"-"`
}

type persistentClientStore interface {
	putClient(*OAuthClient) error
	getClient(clientID string) (*OAuthClient, bool)
}

type persistentTokenStore interface {
	putAccessToken(*TokenEntry) error
	getAccessToken(token string) (*TokenEntry, bool)
	deleteAccessToken(token string) error
	putRefreshToken(*RefreshTokenEntry) error
	getRefreshToken(token string) (*RefreshTokenEntry, bool)
	deleteRefreshToken(token string) error
	rotateRefreshToken(oldRefreshToken string, access *TokenEntry, replacementRefresh *RefreshTokenEntry, oldRefresh *RefreshTokenEntry) error
}

// AuthorizationCode represents a pending authorization code.
type AuthorizationCode struct {
	Code                string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string // "S256"
	State               string
	ExpiresAt           time.Time
	Used                bool
}

// TokenEntry represents an issued access token.
type TokenEntry struct {
	Token        string
	ClientID     string
	ExpiresAt    time.Time
	RefreshToken string // associated refresh token
}

// RefreshTokenEntry represents an issued refresh token.
type RefreshTokenEntry struct {
	Token                      string
	ClientID                   string
	ExpiresAt                  time.Time
	ReplacedAt                 time.Time
	ReplacementAccessToken     string
	ReplacementRefreshToken    string
	ReplacementAccessExpiresAt time.Time
}

// TokenPair represents a freshly issued access and refresh token pair.
type TokenPair struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	Replayed         bool
}

// ClientStore manages registered OAuth clients in memory.
type ClientStore struct {
	mu      sync.RWMutex
	clients map[string]*OAuthClient // keyed by client_id
	persist persistentClientStore
}

func newClientStore() *ClientStore {
	return &ClientStore{clients: make(map[string]*OAuthClient)}
}

func (s *ClientStore) Put(c *OAuthClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c.ClientID] = c
	if s.persist != nil {
		_ = s.persist.putClient(c)
	}
}

func (s *ClientStore) Get(clientID string) (*OAuthClient, bool) {
	s.mu.RLock()
	c, ok := s.clients[clientID]
	s.mu.RUnlock()
	if ok {
		return c, true
	}
	if s.persist != nil {
		c, ok = s.persist.getClient(clientID)
		if ok {
			s.mu.Lock()
			s.clients[clientID] = c
			s.mu.Unlock()
		}
	}
	return c, ok
}

// CodeStore manages authorization codes in memory.
type CodeStore struct {
	mu    sync.RWMutex
	codes map[string]*AuthorizationCode // keyed by code
}

func newCodeStore() *CodeStore {
	return &CodeStore{codes: make(map[string]*AuthorizationCode)}
}

func (s *CodeStore) Put(c *AuthorizationCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[c.Code] = c
}

func (s *CodeStore) Get(code string) (*AuthorizationCode, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.codes[code]
	return c, ok
}

func (s *CodeStore) MarkUsed(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.codes[code]; ok {
		c.Used = true
	}
}

func (s *CodeStore) Delete(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.codes, code)
}

// cleanup removes expired codes.
func (s *CodeStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, c := range s.codes {
		if now.After(c.ExpiresAt) {
			delete(s.codes, k)
		}
	}
}

// TokenStore manages access tokens and refresh tokens in memory.
type TokenStore struct {
	mu            sync.RWMutex
	accessTokens  map[string]*TokenEntry        // keyed by access token
	refreshTokens map[string]*RefreshTokenEntry // keyed by refresh token
	persist       persistentTokenStore
}

func newTokenStore() *TokenStore {
	return &TokenStore{
		accessTokens:  make(map[string]*TokenEntry),
		refreshTokens: make(map[string]*RefreshTokenEntry),
	}
}

func (s *TokenStore) PutAccessToken(t *TokenEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessTokens[t.Token] = t
	if s.persist != nil {
		_ = s.persist.putAccessToken(t)
	}
}

func (s *TokenStore) GetAccessToken(token string) (*TokenEntry, bool) {
	s.mu.RLock()
	t, ok := s.accessTokens[token]
	s.mu.RUnlock()
	if ok {
		return t, true
	}
	if s.persist != nil {
		t, ok = s.persist.getAccessToken(token)
		if ok {
			s.mu.Lock()
			s.accessTokens[token] = t
			s.mu.Unlock()
		}
	}
	return t, ok
}

func (s *TokenStore) DeleteAccessToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accessTokens, token)
	if s.persist != nil {
		_ = s.persist.deleteAccessToken(token)
	}
}

func (s *TokenStore) PutRefreshToken(t *RefreshTokenEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshTokens[t.Token] = t
	if s.persist != nil {
		_ = s.persist.putRefreshToken(t)
	}
}

func (s *TokenStore) GetRefreshToken(token string) (*RefreshTokenEntry, bool) {
	s.mu.RLock()
	t, ok := s.refreshTokens[token]
	s.mu.RUnlock()
	if ok {
		return t, true
	}
	if s.persist != nil {
		t, ok = s.persist.getRefreshToken(token)
		if ok {
			s.mu.Lock()
			s.refreshTokens[token] = t
			s.mu.Unlock()
		}
	}
	return t, ok
}

func (s *TokenStore) DeleteRefreshToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refreshTokens, token)
	if s.persist != nil {
		_ = s.persist.deleteRefreshToken(token)
	}
}

// RotateRefreshToken atomically rotates a refresh token. The replay grace window
// returns the same replacement pair for stale or concurrent refresh attempts from
// clients that share token cache state imperfectly.
func (s *TokenStore) RotateRefreshToken(refreshToken, clientID string, now time.Time, replayGrace time.Duration, issue func() (*TokenPair, error)) (*TokenPair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rt, ok := s.refreshTokens[refreshToken]
	if !ok && s.persist != nil {
		rt, ok = s.persist.getRefreshToken(refreshToken)
		if ok {
			s.refreshTokens[refreshToken] = rt
		}
	}
	if !ok || now.After(rt.ExpiresAt) {
		return nil, errInvalidRefreshToken
	}
	if rt.ClientID != clientID {
		return nil, errRefreshClientMismatch
	}
	if !rt.ReplacedAt.IsZero() {
		if now.Sub(rt.ReplacedAt) <= replayGrace && rt.ReplacementAccessToken != "" && rt.ReplacementRefreshToken != "" {
			refreshExpiresAt := now.Add(refreshTokenTTL)
			if replacement, ok := s.refreshTokens[rt.ReplacementRefreshToken]; ok {
				refreshExpiresAt = replacement.ExpiresAt
			} else if s.persist != nil {
				if replacement, ok := s.persist.getRefreshToken(rt.ReplacementRefreshToken); ok {
					refreshExpiresAt = replacement.ExpiresAt
					s.refreshTokens[rt.ReplacementRefreshToken] = replacement
				}
			}
			return &TokenPair{
				AccessToken:      rt.ReplacementAccessToken,
				RefreshToken:     rt.ReplacementRefreshToken,
				AccessExpiresAt:  rt.ReplacementAccessExpiresAt,
				RefreshExpiresAt: refreshExpiresAt,
				Replayed:         true,
			}, nil
		}
		return nil, errInvalidRefreshToken
	}

	pair, err := issue()
	if err != nil {
		return nil, err
	}
	access := &TokenEntry{
		Token:        pair.AccessToken,
		ClientID:     clientID,
		ExpiresAt:    pair.AccessExpiresAt,
		RefreshToken: pair.RefreshToken,
	}
	replacementRefresh := &RefreshTokenEntry{
		Token:     pair.RefreshToken,
		ClientID:  clientID,
		ExpiresAt: pair.RefreshExpiresAt,
	}
	rt.ReplacedAt = now
	rt.ReplacementAccessToken = pair.AccessToken
	rt.ReplacementRefreshToken = pair.RefreshToken
	rt.ReplacementAccessExpiresAt = pair.AccessExpiresAt

	if s.persist != nil {
		if err := s.persist.rotateRefreshToken(refreshToken, access, replacementRefresh, rt); err != nil {
			return nil, err
		}
	}
	s.accessTokens[pair.AccessToken] = access
	s.refreshTokens[pair.RefreshToken] = replacementRefresh
	s.refreshTokens[refreshToken] = rt
	return pair, nil
}

// cleanup removes expired access tokens and refresh tokens.
func (s *TokenStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, t := range s.accessTokens {
		if now.After(t.ExpiresAt) {
			delete(s.accessTokens, k)
		}
	}
	for k, t := range s.refreshTokens {
		if now.After(t.ExpiresAt) {
			delete(s.refreshTokens, k)
		}
	}
	if s.persist != nil {
		if store, ok := s.persist.(*persistentStore); ok {
			store.cleanup(now)
		}
	}
}

// startCleanup runs periodic cleanup of expired entries. Returns a stop channel.
func startCleanup(codes *CodeStore, tokens *TokenStore) chan struct{} {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				codes.cleanup()
				tokens.cleanup()
			case <-stop:
				return
			}
		}
	}()
	return stop
}
