// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"sync"
	"time"
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
	Token     string
	ClientID  string
	ExpiresAt time.Time
}

// ClientStore manages registered OAuth clients in memory.
type ClientStore struct {
	mu      sync.RWMutex
	clients map[string]*OAuthClient // keyed by client_id
}

func newClientStore() *ClientStore {
	return &ClientStore{clients: make(map[string]*OAuthClient)}
}

func (s *ClientStore) Put(c *OAuthClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c.ClientID] = c
}

func (s *ClientStore) Get(clientID string) (*OAuthClient, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[clientID]
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
}

func (s *TokenStore) GetAccessToken(token string) (*TokenEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.accessTokens[token]
	return t, ok
}

func (s *TokenStore) DeleteAccessToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accessTokens, token)
}

func (s *TokenStore) PutRefreshToken(t *RefreshTokenEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshTokens[t.Token] = t
}

func (s *TokenStore) GetRefreshToken(token string) (*RefreshTokenEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.refreshTokens[token]
	return t, ok
}

func (s *TokenStore) DeleteRefreshToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refreshTokens, token)
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
