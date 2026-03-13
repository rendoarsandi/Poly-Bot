package api

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strconv"
	"time"
)

type KalshiSigner struct {
	PrivateKey *rsa.PrivateKey
	AccessKey  string
}

// NewKalshiSigner creates a new signer for Kalshi.
// privateKeyPEM should be the unencrypted RSA private key in PEM format.
func NewKalshiSigner(accessKey, privateKeyPEM string) (*KalshiSigner, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block containing the key")
	}

	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try parsing as PKCS8
		parsedKey, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse RSA private key: %v (PKCS8: %v)", err, err2)
		}
		var ok bool
		priv, ok = parsedKey.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("parsed key is not an RSA private key")
		}
	}

	return &KalshiSigner{
		PrivateKey: priv,
		AccessKey:  accessKey,
	}, nil
}

func (s *KalshiSigner) SignRequest(method, path string) (timestamp string, signature string, err error) {
	ts := time.Now().UnixMilli()
	timestamp = strconv.FormatInt(ts, 10)

	msg := timestamp + method + path
	hashed := sha256.Sum256([]byte(msg))

	sigBytes, err := rsa.SignPSS(rand.Reader, s.PrivateKey, crypto.SHA256, hashed[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
	})
	if err != nil {
		return "", "", err
	}

	signature = base64.StdEncoding.EncodeToString(sigBytes)
	return timestamp, signature, nil
}
