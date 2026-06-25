package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

var magicPrefix = []byte{0x1d, 0x73, 0x63, 0x72} // non-printable + "scr"

const (
	magicLen = 4
	keyLen   = 32 // AES-256
)

func KeyPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "reasonix", ".session-key")
}

func loadOrCreateKey() ([]byte, error) {
	path := KeyPath()
	if path == "" {
		return nil, errors.New("sessioncrypt: cannot resolve user config dir")
	}
	if data, err := os.ReadFile(path); err == nil {
		if len(data) == keyLen {
			return data, nil
		}
		backup := path + ".corrupt." + strconv.FormatInt(time.Now().UnixMilli(), 36)
		_ = os.Rename(path, backup)
	}
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("sessioncrypt: generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sessioncrypt: mkdir: %w", err)
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		if _, werr := f.Write(key); werr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("sessioncrypt: write key: %w", werr)
		}
		if cerr := f.Close(); cerr != nil {
			log.Printf("sessioncrypt: close key file: %v", cerr)
		}
	} else if os.IsExist(err) {
		if data, rerr := os.ReadFile(path); rerr == nil && len(data) == keyLen {
			return data, nil
		}
		return nil, fmt.Errorf("sessioncrypt: key file raced and unreadable: %w", err)
	} else {
		return nil, fmt.Errorf("sessioncrypt: create key file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("sessioncrypt: chmod key file: %w", err)
	}
	return key, nil
}

// encryptWithAAD encrypts plaintext with AES-256-GCM, binding aad (additional
// authenticated data) to the ciphertext. Output: magic(4) || nonce(12) || ciphertext || tag.
func encryptWithAAD(plaintext, aad []byte) ([]byte, error) {
	key, err := loadOrCreateKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sessioncrypt: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sessioncrypt: gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("sessioncrypt: nonce: %w", err)
	}
	// Layout: magic || nonce || ciphertext(with tag appended by Seal)
	out := make([]byte, magicLen+nonceSize+len(plaintext)+gcm.Overhead())
	copy(out, magicPrefix)
	copy(out[magicLen:], nonce)
	gcm.Seal(out[magicLen+nonceSize:magicLen+nonceSize], nonce, plaintext, aad)
	return out, nil
}

// Encrypt encrypts plaintext with AES-256-GCM.
// Output: magic(4) || nonce(12) || ciphertext || tag.
func Encrypt(plaintext []byte) ([]byte, error) {
	return encryptWithAAD(plaintext, nil)
}

func DecryptWithAAD(data, aad []byte) ([]byte, error) {
	if len(data) < magicLen {
		return nil, errors.New("sessioncrypt: data too short (missing magic)")
	}
	if !bytes.HasPrefix(data, magicPrefix) {
		return nil, errors.New("sessioncrypt: invalid magic prefix")
	}
	key, err := loadOrCreateKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sessioncrypt: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sessioncrypt: gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	payload := data[magicLen:]
	if len(payload) < nonceSize+gcm.Overhead() {
		return nil, fmt.Errorf("sessioncrypt: data too short")
	}
	nonce, ciphertext := payload[:nonceSize], payload[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("sessioncrypt: decrypt: %w", err)
	}
	return plaintext, nil
}

func Decrypt(data []byte) ([]byte, error) {
	return DecryptWithAAD(data, nil)
}

func IsEncrypted(data []byte) bool {
	return bytes.HasPrefix(data, magicPrefix)
}

// WriteEncryptedFile writes data to path, encrypted with AES-256-GCM.
// If the session key file does not exist, writes plaintext as fallback
// to maintain backward compatibility with environments lacking a key.
func WriteEncryptedFile(path string, data []byte) error {
	encrypted, err := Encrypt(data)
	if err != nil {
		// Key unavailable — write plaintext so the system remains usable.
		return os.WriteFile(path, data, 0o600)
	}
	return os.WriteFile(path, encrypted, 0o600)
}

// ReadEncryptedFile reads a file from path and, if it starts with the
// encryption magic prefix, decrypts it transparently.  Plaintext files
// (legacy or written by reasonix serve) are returned as-is.
func ReadEncryptedFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if IsEncrypted(data) {
		return Decrypt(data)
	}
	return data, nil
}
