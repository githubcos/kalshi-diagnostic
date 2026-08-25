package kalshi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://external-api.kalshi.com/trade-api/v2"

type Client struct {
	BaseURL    string
	APIKeyID   string
	PrivateKey *rsa.PrivateKey
	HTTP       *http.Client
}

func NewClient(baseURL, apiKeyID string, privateKeyPEM []byte) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}

	var privateKey *rsa.PrivateKey

	if len(privateKeyPEM) > 0 {
		block, _ := pem.Decode(privateKeyPEM)
		if block == nil {
			return nil, fmt.Errorf("kalshi: invalid PEM private key")
		}

		if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			rsaKey, ok := key.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("kalshi: private key is not RSA")
			}
			privateKey = rsaKey
		} else {
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("kalshi: unable to parse RSA private key: %w", err)
			}
			privateKey = key
		}
	}

	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKeyID:   apiKeyID,
		PrivateKey: privateKey,
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
		},
	}, nil
}

func (c *Client) sign(timestamp, method, path string) (string, error) {
	path = strings.Split(path, "?")[0]

	message := timestamp + strings.ToUpper(method) + path
	hash := sha256.Sum256([]byte(message))

	sig, err := rsa.SignPSS(
		rand.Reader,
		c.PrivateKey,
		crypto.SHA256,
		hash[:],
		&rsa.PSSOptions{
			SaltLength: rsa.PSSSaltLengthEqualsHash,
			Hash:       crypto.SHA256,
		},
	)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(sig), nil
}

func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader

	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		method,
		c.BaseURL+path,
		reader,
	)
	if err != nil {
		return err
	}

	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

	signature, err := c.sign(
		timestamp,
		method,
		"/trade-api/v2"+strings.Split(path, "?")[0],
	)
	if err != nil {
		return err
	}

	req.Header.Set("KALSHI-ACCESS-KEY", c.APIKeyID)
	req.Header.Set("KALSHI-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("KALSHI-ACCESS-SIGNATURE", signature)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf(
			"kalshi API %s %s returned %d: %s",
			method,
			path,
			resp.StatusCode,
			string(data),
		)
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) DoPublic(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		c.BaseURL+path,
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf(
			"kalshi public API %s %s returned %d: %s",
			method,
			path,
			resp.StatusCode,
			string(data),
		)
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return err
		}
	}

	return nil
}
