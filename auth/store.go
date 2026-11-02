// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"sync"
	"time"
)

var (
	errInvalidRefreshToken   = errors.New("invalid or expired refresh token")
	errRefreshClientMismatch = errors.New("client_id mismatch")
	errInvalidAuthCode       = errors.New("invalid, expired or already used authorization code")
	errReplayUnseal          = errors.New("refresh replay data cannot be decrypted")
)

// hashToken returns the storage key for an opaque token: hex(sha256(token)).
// SHA-256 is sufficient (no salt or stretching) because tokens are 256-bit
// random values, so the hash cannot be brute-forced back to the token.
func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// hashTokenOrEmpty is hashToken that keeps an absent token absent.
func hashTokenOrEmpty(t string) string {
	if t == "" {
		return ""
	}
	return hashToken(t)
}

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

// persistentTokenStore persists token entries. Every token argument is the
// hex SHA-256 hash of the token, never the token itself.
type persistentTokenStore interface {
	putAccessToken(*TokenEntry) error
	getAccessToken(tokenHash string) (*TokenEntry, bool)
	deleteAccessToken(tokenHash string) error
	putRefreshToken(*RefreshTokenEntry) error
	getRefreshToken(tokenHash string) (*RefreshTokenEntry, bool)
	deleteRefreshToken(tokenHash string) error
	deleteAccessTokensByRefresh(refreshHash string) error
	findReplacedRefresh(replacementRefreshHash string) (string, bool)
	rotateRefreshToken(oldRefreshHash string, access *TokenEntry, replacementRefresh *RefreshTokenEntry, oldRefresh *RefreshTokenEntry) error
}

// AuthorizationCode represents a pending authorization code.
type AuthorizationCode struct {
	Code                string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string // "S256"
	Resource            string // RFC 8707 resource requested at /authorize (trailing slash trimmed)
	State               string
	ExpiresAt           time.Time
	Used                bool
}

// TokenEntry represents an issued access token. It holds only hashes.
type TokenEntry struct {
	TokenHash   string
	ClientID    string
	ExpiresAt   time.Time
	RefreshHash string // hash of the associated refresh token, "" if none
}

// RefreshTokenEntry represents an issued refresh token. It holds only hashes;
// the replacement pair needed for replay is sealed with a key derived from the
// (now rotated) refresh token itself.
type RefreshTokenEntry struct {
	TokenHash                  string
	ClientID                   string
	ExpiresAt                  time.Time
	ReplacedAt                 time.Time
	ReplacementAccessHash      string
	ReplacementRefreshHash     string
	ReplacementSealed          []byte
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

// Put stores a client. When a persistent store is configured the client is
// written there first, and a failure is returned instead of being dropped.
func (s *ClientStore) Put(c *OAuthClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persist != nil {
		if err := s.persist.putClient(c); err != nil {
			return err
		}
	}
	s.clients[c.ClientID] = c
	return nil
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

// Consume atomically validates and redeems an authorization code: under one
// lock it checks existence, the Used flag and expiry, then marks the code
// used. Exactly one concurrent caller can succeed. The returned value is a copy.
func (s *CodeStore) Consume(code string, now time.Time) (*AuthorizationCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[code]
	if !ok || c.Used || now.After(c.ExpiresAt) {
		return nil, errInvalidAuthCode
	}
	c.Used = true
	cp := *c
	return &cp, nil
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

// TokenStore manages access tokens and refresh tokens. Callers pass plaintext
// tokens; everything stored (in memory or durably) is keyed by their hash.
type TokenStore struct {
	mu            sync.RWMutex
	accessTokens  map[string]*TokenEntry        // keyed by access token hash
	refreshTokens map[string]*RefreshTokenEntry // keyed by refresh token hash
	persist       persistentTokenStore
}

func newTokenStore() *TokenStore {
	return &TokenStore{
		accessTokens:  make(map[string]*TokenEntry),
		refreshTokens: make(map[string]*RefreshTokenEntry),
	}
}

// PutAccessToken stores an access token linked to refreshToken ("" for none).
// It fails if the token could not be persisted, so that the caller does not hand
// out a token that would vanish on restart.
func (s *TokenStore) PutAccessToken(token, clientID string, expiresAt time.Time, refreshToken string) error {
	t := &TokenEntry{
		TokenHash:   hashToken(token),
		ClientID:    clientID,
		ExpiresAt:   expiresAt,
		RefreshHash: hashTokenOrEmpty(refreshToken),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persist != nil {
		if err := s.persist.putAccessToken(t); err != nil {
			return err
		}
	}
	s.accessTokens[t.TokenHash] = t
	return nil
}

func (s *TokenStore) GetAccessToken(token string) (*TokenEntry, bool) {
	h := hashToken(token)
	s.mu.RLock()
	t, ok := s.accessTokens[h]
	s.mu.RUnlock()
	if ok {
		return t, true
	}
	if s.persist == nil {
		return nil, false
	}
	if _, ok := s.persist.getAccessToken(h); !ok {
		return nil, false
	}
	// Fill the cache from a read made under the write lock: an earlier read could
	// have been overtaken by a revocation, and caching it would undo that.
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok = s.persist.getAccessToken(h)
	if ok {
		s.accessTokens[h] = t
	}
	return t, ok
}

func (s *TokenStore) DeleteAccessToken(token string) {
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accessTokens, h)
	if s.persist != nil {
		logStoreDelete(s.persist.deleteAccessToken(h))
	}
}

// PutRefreshToken stores a refresh token. Like PutAccessToken, it fails if the
// token could not be persisted.
func (s *TokenStore) PutRefreshToken(token, clientID string, expiresAt time.Time) error {
	t := &RefreshTokenEntry{
		TokenHash: hashToken(token),
		ClientID:  clientID,
		ExpiresAt: expiresAt,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persist != nil {
		if err := s.persist.putRefreshToken(t); err != nil {
			return err
		}
	}
	s.refreshTokens[t.TokenHash] = t
	return nil
}

func (s *TokenStore) GetRefreshToken(token string) (*RefreshTokenEntry, bool) {
	h := hashToken(token)
	s.mu.RLock()
	t, ok := s.refreshTokens[h]
	s.mu.RUnlock()
	if ok {
		return t, true
	}
	if s.persist == nil {
		return nil, false
	}
	if _, ok := s.persist.getRefreshToken(h); !ok {
		return nil, false
	}
	// Under the write lock for the same reason as in GetAccessToken.
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok = s.persist.getRefreshToken(h)
	if ok {
		s.refreshTokens[h] = t
	}
	return t, ok
}

func (s *TokenStore) DeleteRefreshToken(token string) {
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refreshTokens, h)
	if s.persist != nil {
		logStoreDelete(s.persist.deleteRefreshToken(h))
	}
}

// logStoreDelete reports a failed delete. A revoked token gone from memory but
// not from the store comes back on restart, so the operator has to know.
func logStoreDelete(err error) {
	if err != nil {
		log.Printf("OAuth store: failed to delete a revoked token, it stays valid after a restart: %v", err)
	}
}

// lookupRefreshLocked finds a refresh token by hash in memory or the persistent store.
func (s *TokenStore) lookupRefreshLocked(hash string) (*RefreshTokenEntry, bool) {
	if rt, ok := s.refreshTokens[hash]; ok {
		return rt, true
	}
	if s.persist != nil {
		return s.persist.getRefreshToken(hash)
	}
	return nil, false
}

// predecessorLocked returns the hash of the refresh token that was rotated
// into the refresh token with the given hash.
func (s *TokenStore) predecessorLocked(hash string) string {
	for k, rt := range s.refreshTokens {
		if rt.ReplacementRefreshHash == hash {
			return k
		}
	}
	if s.persist != nil {
		if k, ok := s.persist.findReplacedRefresh(hash); ok {
			return k
		}
	}
	return ""
}

// RevokeRefreshToken revokes a refresh token together with every access token
// linked to it (RFC 7009 section 2.1). Because rotation chains refresh tokens,
// the whole chain (earlier and later members) is revoked, so a replay inside
// the grace window cannot resurrect the session.
func (s *TokenStore) RevokeRefreshToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	start := hashToken(token)
	chain := map[string]bool{}
	for cur := start; cur != "" && !chain[cur]; {
		chain[cur] = true
		rt, ok := s.lookupRefreshLocked(cur)
		if !ok {
			break
		}
		cur = rt.ReplacementRefreshHash
	}
	for cur := start; cur != ""; {
		pred := s.predecessorLocked(cur)
		if pred == "" || chain[pred] {
			break
		}
		chain[pred] = true
		cur = pred
	}
	for rth := range chain {
		for k, at := range s.accessTokens {
			if at.RefreshHash == rth {
				delete(s.accessTokens, k)
			}
		}
		delete(s.refreshTokens, rth)
		if s.persist != nil {
			logStoreDelete(s.persist.deleteAccessTokensByRefresh(rth))
			logStoreDelete(s.persist.deleteRefreshToken(rth))
		}
	}
}

// RotateRefreshToken atomically rotates a refresh token. The replay grace window
// returns the same replacement pair for stale or concurrent refresh attempts from
// clients that share token cache state imperfectly. The replacement pair is kept
// only in sealed form, decryptable with the rotated refresh token.
func (s *TokenStore) RotateRefreshToken(refreshToken, clientID string, now time.Time, replayGrace time.Duration, issue func() (*TokenPair, error)) (*TokenPair, error) {
	h := hashToken(refreshToken)
	s.mu.Lock()
	defer s.mu.Unlock()

	rt, ok := s.refreshTokens[h]
	if !ok && s.persist != nil {
		rt, ok = s.persist.getRefreshToken(h)
		if ok {
			s.refreshTokens[h] = rt
		}
	}
	if !ok || now.After(rt.ExpiresAt) {
		return nil, errInvalidRefreshToken
	}
	if rt.ClientID != clientID {
		return nil, errRefreshClientMismatch
	}
	if !rt.ReplacedAt.IsZero() {
		if now.Sub(rt.ReplacedAt) <= replayGrace && len(rt.ReplacementSealed) > 0 {
			access, refresh, err := unsealReplacement(refreshToken, rt.ReplacementSealed)
			if err != nil {
				return nil, errReplayUnseal
			}
			refreshExpiresAt := now.Add(refreshTokenTTL)
			if replacement, ok := s.lookupRefreshLocked(rt.ReplacementRefreshHash); ok {
				refreshExpiresAt = replacement.ExpiresAt
				s.refreshTokens[rt.ReplacementRefreshHash] = replacement
			}
			return &TokenPair{
				AccessToken:      access,
				RefreshToken:     refresh,
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
	sealed, err := sealReplacement(refreshToken, pair.AccessToken, pair.RefreshToken)
	if err != nil {
		return nil, err
	}
	access := &TokenEntry{
		TokenHash:   hashToken(pair.AccessToken),
		ClientID:    clientID,
		ExpiresAt:   pair.AccessExpiresAt,
		RefreshHash: hashToken(pair.RefreshToken),
	}
	replacementRefresh := &RefreshTokenEntry{
		TokenHash: hashToken(pair.RefreshToken),
		ClientID:  clientID,
		ExpiresAt: pair.RefreshExpiresAt,
	}
	rotated := *rt
	rotated.ReplacedAt = now
	rotated.ReplacementAccessHash = access.TokenHash
	rotated.ReplacementRefreshHash = replacementRefresh.TokenHash
	rotated.ReplacementSealed = sealed
	rotated.ReplacementAccessExpiresAt = pair.AccessExpiresAt

	if s.persist != nil {
		if err := s.persist.rotateRefreshToken(h, access, replacementRefresh, &rotated); err != nil {
			return nil, err
		}
	}
	s.accessTokens[access.TokenHash] = access
	s.refreshTokens[replacementRefresh.TokenHash] = replacementRefresh
	s.refreshTokens[h] = &rotated
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
func startCleanup(codes *CodeStore, tokens *TokenStore, extra ...func()) chan struct{} {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				codes.cleanup()
				tokens.cleanup()
				for _, fn := range extra {
					fn()
				}
			case <-stop:
				return
			}
		}
	}()
	return stop
}
