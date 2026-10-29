// Package litellm provides the subset of the LiteLLM key-management API used by
// the operator. Returned values contain secrets and must not be logged.
package litellm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// APIError reports an endpoint failure without exposing request, response, or
// transport details. StatusCode is zero when no HTTP response was received.
// Only caller context errors are available through Unwrap.
type APIError struct {
	Path       string
	StatusCode int
	message    string
	cause      error
}

func (e *APIError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("%s: HTTP status unavailable: %s", e.Path, e.message)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Path, e.StatusCode, e.message)
}

// Unwrap preserves context cancellation and deadlines, never raw transport errors.
func (e *APIError) Unwrap() error { return e.cause }

// Client authenticates key-management requests with a proxy master key.
type Client struct {
	baseURL    string
	masterKey  string
	httpClient *http.Client
}

// New creates a client. A nil httpClient uses http.DefaultClient. Timeouts are
// controlled by the supplied client and request contexts. Redirects are rejected
// to avoid forwarding credential-bearing requests; the supplied client is copied.
func New(baseURL, masterKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := *httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), masterKey: masterKey, httpClient: &client}
}

// KeyFields contains the managed attributes of a virtual key.
type KeyFields struct {
	KeyAlias string            `json:"key_alias"`
	Models   []string          `json:"models"`
	UserID   string            `json:"user_id,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// KeyInfo contains managed attributes and a stored hash, when returned by the API.
type KeyInfo struct {
	KeyAlias string   `json:"key_alias"`
	Models   []string `json:"models"`
	UserID   string   `json:"user_id,omitempty"`
	// Metadata keeps numbers as json.Number so updates preserve their exact values.
	Metadata map[string]any `json:"metadata,omitempty"`
	// TokenHash is secret material. LiteLLM v1.93.2 removes token from
	// /v2/key/info responses, so this field is empty for that endpoint.
	TokenHash string `json:"token,omitempty"`
}

// GenerateKeyRequest creates a key with a caller-supplied virtual key value.
type GenerateKeyRequest struct {
	KeyFields
	Key string `json:"key"`
}

// GeneratedKey includes the raw virtual key returned on creation.
type GeneratedKey struct {
	KeyInfo
	Key string `json:"key"`
}

// UpdateKeyRequest is a partial update. Nil fields are omitted; pointers to empty
// values explicitly clear a field. An alias-only update preserves other fields.
// LiteLLM replaces supplied metadata, so include existing unmanaged JSON values.
// LiteLLM treats an empty models list as allowing all models.
type UpdateKeyRequest struct {
	KeyAlias *string         `json:"key_alias,omitempty"`
	Models   *[]string       `json:"models,omitempty"`
	UserID   *string         `json:"user_id,omitempty"`
	Metadata *map[string]any `json:"metadata,omitempty"`
}

// DeleteKeysRequest selects keys by alias or by raw key/stored hash. If both
// selectors are supplied, LiteLLM gives Keys precedence over KeyAliases.
type DeleteKeysRequest struct {
	KeyAliases []string `json:"key_aliases,omitempty"`
	Keys       []string `json:"keys,omitempty"`
}

// DeleteKeysResponse echoes the selectors deleted by LiteLLM and may hold secrets.
type DeleteKeysResponse struct {
	DeletedKeys []string `json:"deleted_keys"`
}

// KeyInfo looks up one alias. An empty info array means not found and returns
// nil, nil. HTTP errors (including 404) remain errors. The token hash is absent
// in upstream v1.93.2; callers must not interpret an empty hash as a rotation.
func (c *Client) KeyInfo(ctx context.Context, alias string) (*KeyInfo, error) {
	request := struct {
		KeyAliases []string `json:"key_aliases"`
	}{KeyAliases: []string{alias}}
	return c.keyInfo(ctx, request, alias)
}

// KeyInfoByValue looks up a raw virtual key using a POST body, never a URL.
// A missing value returns nil, nil. Results must contain exactly one named alias.
func (c *Client) KeyInfoByValue(ctx context.Context, value string) (*KeyInfo, error) {
	request := struct {
		Keys []string `json:"keys"`
	}{Keys: []string{value}}
	return c.keyInfo(ctx, request, "")
}

func (c *Client) keyInfo(ctx context.Context, request any, expectedAlias string) (*KeyInfo, error) {
	const path = "/v2/key/info"
	var response struct {
		Info []KeyInfo `json:"info"`
	}
	status, err := c.post(ctx, path, request, &response)
	if err != nil {
		return nil, err
	}
	if response.Info == nil {
		return nil, responseError(path, status)
	}
	if len(response.Info) == 0 {
		return nil, nil
	}
	if len(response.Info) != 1 || response.Info[0].KeyAlias == "" {
		return nil, responseError(path, status)
	}
	if expectedAlias != "" && response.Info[0].KeyAlias != expectedAlias {
		return nil, responseError(path, status)
	}
	return &response.Info[0], nil
}

// GenerateKey creates a virtual key without retries.
func (c *Client) GenerateKey(ctx context.Context, request GenerateKeyRequest) (*GeneratedKey, error) {
	const path = "/key/generate"
	var response GeneratedKey
	status, err := c.post(ctx, path, request, &response)
	if err != nil {
		return nil, err
	}
	if response.Key == "" {
		return nil, responseError(path, status)
	}
	return &response, nil
}

// UpdateKey changes managed fields, including the alias. The identifier is a raw
// key or stored token hash, encoded as LiteLLM's required "key" request field.
func (c *Client) UpdateKey(ctx context.Context, identifier string, update UpdateKeyRequest) (*KeyInfo, error) {
	request := struct {
		UpdateKeyRequest
		Key string `json:"key"`
	}{UpdateKeyRequest: update, Key: identifier}
	const path = "/key/update"
	var response *KeyInfo
	status, err := c.post(ctx, path, request, &response)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, responseError(path, status)
	}
	return response, nil
}

// DeleteKeys deletes the selected keys without retries. Missing keys can produce
// an upstream 404, which is returned as an error rather than silently ignored.
func (c *Client) DeleteKeys(ctx context.Context, request DeleteKeysRequest) (*DeleteKeysResponse, error) {
	const path = "/key/delete"
	var response DeleteKeysResponse
	status, err := c.post(ctx, path, request, &response)
	if err != nil {
		return nil, err
	}
	if response.DeletedKeys == nil {
		return nil, responseError(path, status)
	}
	return &response, nil
}

// TokenHash matches LiteLLM's SHA-256 hex digest of the entire UTF-8 key,
// including the "sk-" prefix. Its result is secret material and must not be logged.
func TokenHash(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func responseError(path string, status int) *APIError {
	return &APIError{Path: path, StatusCode: status, message: "invalid response"}
}

func (c *Client) post(ctx context.Context, path string, input, output any) (int, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return 0, &APIError{Path: path, message: "could not encode request"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, &APIError{Path: path, message: "could not create request"}
	}
	request.Header.Set("Authorization", "Bearer "+c.masterKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		// Do not wrap transport errors: they can contain URLs or arbitrary secret text.
		return 0, &APIError{Path: path, message: "request failed", cause: ctx.Err()}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// Neither the body nor the server-supplied status text is safe to expose.
		return response.StatusCode, &APIError{Path: path, StatusCode: response.StatusCode, message: "request failed"}
	}
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		failure := responseError(path, response.StatusCode)
		failure.cause = ctx.Err()
		return response.StatusCode, failure
	}
	return response.StatusCode, nil
}
