package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
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
		_ = f.Close()
	} else if os.IsExist(err) {
		if data, rerr := os.ReadFile(path); rerr == nil && len(data) == keyLen {
			return data, nil
		}
		return nil, fmt.Errorf("sessioncrypt: key file raced and unreadable: %w", err)
	} else {
		return nil, fmt.Errorf("sessioncrypt: create key file: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return key, nil
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
