// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
)

const replayKDFInfo = "mms oauth refresh replay v1"

type replayPair struct {
	Access  string `json:"a"`
	Refresh string `json:"r"`
}

// replayAEAD derives the AES-256-GCM key for sealing a replacement pair from
// the plaintext of the refresh token it replaced. Tokens are 256-bit random,
// so HKDF without a salt is sufficient.
func replayAEAD(oldRefreshToken string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(oldRefreshToken), nil, replayKDFInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealReplacement encrypts the replacement access+refresh pair so that only
// the holder of oldRefreshToken can recover it. Output is nonce || ciphertext;
// the old token's hash is bound as additional authenticated data.
func sealReplacement(oldRefreshToken, access, refresh string) ([]byte, error) {
	aead, err := replayAEAD(oldRefreshToken)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(replayPair{Access: access, Refresh: refresh})
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, []byte(hashToken(oldRefreshToken))), nil
}

// unsealReplacement reverses sealReplacement. Any failure (wrong token,
// tampering, truncation) returns an error without revealing why.
func unsealReplacement(oldRefreshToken string, sealed []byte) (access, refresh string, err error) {
	aead, err := replayAEAD(oldRefreshToken)
	if err != nil {
		return "", "", err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", "", errors.New("sealed data too short")
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ct, []byte(hashToken(oldRefreshToken)))
	if err != nil {
		return "", "", err
	}
	var p replayPair
	if err := json.Unmarshal(plain, &p); err != nil || p.Access == "" || p.Refresh == "" {
		return "", "", errors.New("malformed sealed data")
	}
	return p.Access, p.Refresh, nil
}
