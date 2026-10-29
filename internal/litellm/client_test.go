package litellm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alterminder/litellm-virtualkey-operator/internal/litellm"
)

const (
	metadataOwner      = "owner"
	metadataOwnerValue = "example-team"
	aliasRequestBody   = `{"key_aliases":["example-key"]}`
	deletePath         = "/key/delete"
	updatePath         = "/key/update"
	generatePath       = "/key/generate"
	infoPath           = "/v2/key/info"
	masterKey          = "sk-master-test-secret"
	virtualKey         = "sk-test-key"
	tokenHash          = "0d62f396c1317066f55a96086517047c737087c61eb2bf016b72e6298927b15b"
	alias              = "example-key"
)

func TestTokenHash(t *testing.T) {
	// Independently calculated with Python hashlib.sha256(b"sk-test-key").hexdigest().
	if got := litellm.TokenHash(virtualKey); got != tokenHash {
		t.Fatalf("TokenHash = %q, want %q", got, tokenHash)
	}
}

func TestEndpointContracts(t *testing.T) {
	fields := litellm.KeyFields{
		KeyAlias: alias, Models: []string{"model-a", "model-b"}, UserID: "example-user",
		Metadata: map[string]string{metadataOwner: metadataOwnerValue},
	}
	info := litellm.KeyInfo{
		KeyAlias: fields.KeyAlias, Models: fields.Models, UserID: fields.UserID,
		Metadata: map[string]any{metadataOwner: metadataOwnerValue},
	}
	mergedMetadata := map[string]any{
		metadataOwner: metadataOwnerValue,
		"guardrails":  []any{"pii"},
		"settings":    map[string]any{"enabled": true},
	}
	withHash := info
	withHash.TokenHash = tokenHash
	cases := []struct {
		name     string
		path     string
		body     string
		response string
		call     func(*litellm.Client) (any, error)
		want     any
	}{
		{
			name: "info upstream omits hash", path: infoPath, body: aliasRequestBody,
			response: `{"key":null,"info":[{"key_alias":"example-key","models":["model-a","model-b"],"user_id":"example-user","metadata":{"owner":"example-team"},"spend":0}]}`,
			call:     func(c *litellm.Client) (any, error) { return c.KeyInfo(t.Context(), alias) }, want: &info,
		},
		{
			name: "info optional hash", path: infoPath, body: aliasRequestBody,
			response: `{"info":[{"token":"` + tokenHash + `","key_alias":"example-key","models":["model-a","model-b"],"user_id":"example-user","metadata":{"owner":"example-team"}}]}`,
			call:     func(c *litellm.Client) (any, error) { return c.KeyInfo(t.Context(), alias) }, want: &withHash,
		},
		{
			name: "info by value", path: infoPath, body: `{"keys":["sk-test-key"]}`,
			response: `{"key":["sk-test-key"],"info":[{"key_alias":"example-key","models":["model-a","model-b"],"user_id":"example-user","metadata":{"owner":"example-team"}}]}`,
			call:     func(c *litellm.Client) (any, error) { return c.KeyInfoByValue(t.Context(), virtualKey) }, want: &info,
		},
		{
			name: "generate", path: generatePath,
			body:     `{"key":"sk-test-key","key_alias":"example-key","models":["model-a","model-b"],"user_id":"example-user","metadata":{"owner":"example-team"}}`,
			response: `{"key":"sk-test-key","token":"` + tokenHash + `","key_alias":"example-key","models":["model-a","model-b"],"user_id":"example-user","metadata":{"owner":"example-team"}}`,
			call: func(c *litellm.Client) (any, error) {
				return c.GenerateKey(t.Context(), litellm.GenerateKeyRequest{Key: virtualKey, KeyFields: fields})
			}, want: &litellm.GeneratedKey{Key: virtualKey, KeyInfo: withHash},
		},
		{
			name: "update by hash", path: updatePath,
			body:     `{"key":"` + tokenHash + `","key_alias":"example-key","models":["model-a","model-b"],"user_id":"example-user","metadata":{"owner":"example-team"}}`,
			response: `{"key":"` + tokenHash + `","token":"` + tokenHash + `","key_alias":"example-key","models":["model-a","model-b"],"user_id":"example-user","metadata":{"owner":"example-team"}}`,
			call: func(c *litellm.Client) (any, error) {
				return c.UpdateKey(t.Context(), tokenHash, litellm.UpdateKeyRequest{
					KeyAlias: &fields.KeyAlias, Models: &fields.Models, UserID: &fields.UserID, Metadata: &info.Metadata,
				})
			}, want: &withHash,
		},
		{
			name: "update merged JSON metadata", path: updatePath,
			body:     `{"key":"sk-test-key","metadata":{"owner":"example-team","guardrails":["pii"],"settings":{"enabled":true}}}`,
			response: `{"key_alias":"example-key","metadata":{"owner":"example-team","guardrails":["pii"],"settings":{"enabled":true}}}`,
			call: func(c *litellm.Client) (any, error) {
				return c.UpdateKey(t.Context(), virtualKey, litellm.UpdateKeyRequest{Metadata: &mergedMetadata})
			}, want: &litellm.KeyInfo{KeyAlias: alias, Metadata: mergedMetadata},
		},
		{
			name: "rename only", path: updatePath, body: `{"key":"sk-test-key","key_alias":"retired-key"}`,
			response: `{"key_alias":"retired-key"}`,
			call: func(c *litellm.Client) (any, error) {
				return c.UpdateKey(t.Context(), virtualKey, litellm.UpdateKeyRequest{KeyAlias: new("retired-key")})
			}, want: &litellm.KeyInfo{KeyAlias: "retired-key"},
		},
		{
			name: "clear optional fields", path: updatePath,
			body:     `{"key":"sk-test-key","models":[],"user_id":"","metadata":{}}`,
			response: `{"models":[],"user_id":"","metadata":{}}`,
			call: func(c *litellm.Client) (any, error) {
				return c.UpdateKey(t.Context(), virtualKey, litellm.UpdateKeyRequest{
					Models: new([]string{}), UserID: new(""), Metadata: new(map[string]any{}),
				})
			}, want: &litellm.KeyInfo{Models: []string{}, Metadata: map[string]any{}},
		},
		{
			name: "delete alias", path: deletePath, body: aliasRequestBody,
			response: `{"deleted_keys":["example-key"]}`,
			call: func(c *litellm.Client) (any, error) {
				return c.DeleteKeys(t.Context(), litellm.DeleteKeysRequest{KeyAliases: []string{alias}})
			}, want: &litellm.DeleteKeysResponse{DeletedKeys: []string{alias}},
		},
		{
			name: "delete raw key and hash", path: deletePath, body: `{"keys":["sk-test-key","` + tokenHash + `"]}`,
			response: `{"deleted_keys":["sk-test-key","` + tokenHash + `"]}`,
			call: func(c *litellm.Client) (any, error) {
				return c.DeleteKeys(t.Context(), litellm.DeleteKeysRequest{Keys: []string{virtualKey, tokenHash}})
			}, want: &litellm.DeleteKeysResponse{DeletedKeys: []string{virtualKey, tokenHash}},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != tt.path {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, tt.path)
				}
				if r.URL.RawQuery != "" {
					t.Error("request has unexpected URL query parameters")
				}
				if r.Header.Get("Authorization") != "Bearer "+masterKey {
					t.Error("incorrect Authorization header")
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect Content-Type header")
				}
				var got, want any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if err := json.Unmarshal([]byte(tt.body), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("body = %#v, want %#v", got, want)
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(tt.response)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			got, err := tt.call(litellm.New(server.URL+"/", masterKey, server.Client()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("response = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestKeyInfoMissingAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"key":null,"info":[]}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	got, err := litellm.New(server.URL, masterKey, nil).KeyInfo(t.Context(), "missing")
	if err != nil || got != nil {
		t.Fatalf("missing alias = %#v, %v; want nil, nil", got, err)
	}
}

type endpointCall struct {
	path string
	call func(*litellm.Client) error
}

func endpointCalls(ctx context.Context) []endpointCall {
	return []endpointCall{
		{infoPath, func(c *litellm.Client) error { _, err := c.KeyInfo(ctx, alias); return err }},
		{infoPath, func(c *litellm.Client) error { _, err := c.KeyInfoByValue(ctx, virtualKey); return err }},
		{generatePath, func(c *litellm.Client) error {
			_, err := c.GenerateKey(ctx, litellm.GenerateKeyRequest{Key: virtualKey})
			return err
		}},
		{updatePath, func(c *litellm.Client) error {
			_, err := c.UpdateKey(ctx, tokenHash, litellm.UpdateKeyRequest{})
			return err
		}},
		{deletePath, func(c *litellm.Client) error {
			_, err := c.DeleteKeys(ctx, litellm.DeleteKeysRequest{Keys: []string{virtualKey, tokenHash}})
			return err
		}},
	}
}

func assertSafeError(t *testing.T, err error, path, status string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{masterKey, virtualKey, tokenHash} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error contains secret material")
		}
	}
	var apiErr *litellm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error has type %T, want *litellm.APIError", err)
	}
	wantStatus := 0
	if status != "status unavailable" {
		var parseErr error
		wantStatus, parseErr = strconv.Atoi(status)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
	}
	if apiErr.Path != path || apiErr.StatusCode != wantStatus {
		t.Errorf("typed error = %s HTTP %d, want %s HTTP %d", apiErr.Path, apiErr.StatusCode, path, wantStatus)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), status) {
		t.Errorf("error = %q, want endpoint %q and status %q", err, path, status)
	}
}

func TestErrorsDoNotLeakSecrets(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
				if _, err := fmt.Fprintf(w, `{"error":"%s %s %s"}`, masterKey, virtualKey, tokenHash); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			for _, endpoint := range endpointCalls(t.Context()) {
				assertSafeError(t, endpoint.call(litellm.New(server.URL, masterKey, nil)), endpoint.path, fmt.Sprint(status))
			}
			if requests.Load() != 5 {
				t.Errorf("got %d requests, want 5 (no retries)", requests.Load())
			}
		})
	}
}

func TestTransportErrorsDoNotLeakSecrets(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	// Transport errors normally include the URL; deliberately put all secrets there.
	baseURL := server.URL + "/" + masterKey + "/" + virtualKey + "/" + tokenHash
	for _, endpoint := range endpointCalls(t.Context()) {
		assertSafeError(t, endpoint.call(litellm.New(baseURL, masterKey, nil)), endpoint.path, "status unavailable")
	}
}

func TestInvalidResponsesDoNotLeakSecrets(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{"models":"` + masterKey + virtualKey + tokenHash + `"}`,
		`{"metadata":["` + masterKey + virtualKey + tokenHash + `"]}`,
		`{"` + masterKey + virtualKey + tokenHash + `":`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := w.Write([]byte(body)); err != nil {
				t.Error(err)
			}
		}))
		for _, endpoint := range endpointCalls(t.Context()) {
			assertSafeError(t, endpoint.call(litellm.New(server.URL, masterKey, nil)), endpoint.path, "200")
		}
		server.Close()
	}
}

func TestCallerControlsTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	t.Run("context", func(t *testing.T) {
		for i, endpoint := range endpointCalls(t.Context()) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			err := endpointCalls(ctx)[i].call(litellm.New(server.URL, masterKey, nil))
			cancel()
			assertSafeError(t, err, endpoint.path, "status unavailable")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error does not wrap context deadline: %v", err)
			}
		}
	})
	t.Run("client", func(t *testing.T) {
		client := server.Client()
		client.Timeout = 20 * time.Millisecond
		_, err := litellm.New(server.URL, masterKey, client).KeyInfo(t.Context(), alias)
		assertSafeError(t, err, infoPath, "status unavailable")
	})
}

func TestRedirectIsNotFollowed(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("redirect forwarded a credential-bearing request")
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := server.Client()
	for _, endpoint := range endpointCalls(t.Context()) {
		assertSafeError(t, endpoint.call(litellm.New(server.URL, masterKey, client)), endpoint.path, "307")
	}
	if client.CheckRedirect != nil {
		t.Error("New mutated the caller's HTTP client")
	}
}

func TestStructuredResponseMetadata(t *testing.T) {
	metadata := `{"owner":"example-team","guardrails":["pii"],"settings":{"enabled":true},"weight":1,"nullable":null}`
	for _, path := range []string{infoPath, updatePath} {
		t.Run(path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body := `{"key_alias":"example-key","metadata":` + metadata + `}`
				if path == infoPath {
					body = `{"info":[` + body + `]}`
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client := litellm.New(server.URL, masterKey, nil)
			var got *litellm.KeyInfo
			var err error
			if path == infoPath {
				got, err = client.KeyInfo(t.Context(), alias)
			} else {
				got, err = client.UpdateKey(t.Context(), virtualKey, litellm.UpdateKeyRequest{})
			}
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				metadataOwner: metadataOwnerValue, "guardrails": []any{"pii"},
				"settings": map[string]any{"enabled": true}, "weight": json.Number("1"), "nullable": nil,
			}
			if !reflect.DeepEqual(got.Metadata, want) {
				t.Errorf("metadata = %#v, want %#v", got.Metadata, want)
			}
		})
	}
}

func TestKeyInfoByValueResults(t *testing.T) {
	cases := []struct {
		name      string
		response  string
		wantAlias string
		wantError bool
	}{
		{name: "missing", response: `{"info":[]}`},
		{name: "temporary alias", response: `{"info":[{"key_alias":"example-key-rotating"}]}`, wantAlias: alias + "-rotating"},
		{name: "no info", response: `{}`, wantError: true},
		{name: "multiple entries", response: `{"info":[{"key_alias":"a"},{"key_alias":"b"}]}`, wantError: true},
		{name: "missing alias", response: `{"info":[{}]}`, wantError: true},
		{name: "empty alias", response: `{"info":[{"key_alias":""}]}`, wantError: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write([]byte(tt.response)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			got, err := litellm.New(server.URL, masterKey, nil).KeyInfoByValue(t.Context(), virtualKey)
			if tt.wantError {
				assertSafeError(t, err, infoPath, "200")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantAlias == "" {
				if got != nil {
					t.Fatalf("missing key returned a result: %#v", got)
				}
			} else if got == nil || got.KeyAlias != tt.wantAlias {
				t.Fatalf("result = %#v, want alias %s", got, tt.wantAlias)
			}
		})
	}
}

func TestCanceledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a canceled request reached the server")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, endpoint := range endpointCalls(ctx) {
		err := endpoint.call(litellm.New(server.URL, masterKey, nil))
		assertSafeError(t, err, endpoint.path, "status unavailable")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error does not wrap context cancellation: %v", err)
		}
	}
}

func TestRequestCreationErrorsDoNotLeakSecrets(t *testing.T) {
	baseURL := "://" + masterKey + virtualKey + tokenHash
	for _, endpoint := range endpointCalls(t.Context()) {
		assertSafeError(t, endpoint.call(litellm.New(baseURL, masterKey, nil)), endpoint.path, "status unavailable")
	}
}

func TestDeadlineDuringResponseRead(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := litellm.New(server.URL, masterKey, nil).KeyInfoByValue(ctx, virtualKey)
	assertSafeError(t, err, infoPath, "200")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("response read error does not wrap context deadline: %v", err)
	}
}

func TestMetadataIntegerSurvivesInfoUpdateRoundTrip(t *testing.T) {
	var updates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case infoPath:
			if _, err := w.Write([]byte(`{"info":[{"key_alias":"example-key","metadata":{"unmanaged_integer":9007199254740993}}]}`)); err != nil {
				t.Error(err)
			}
		case updatePath:
			updates.Add(1)
			var request struct {
				Metadata map[string]json.RawMessage `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if got := string(request.Metadata["unmanaged_integer"]); got != "9007199254740993" {
				t.Errorf("raw metadata integer = %s, want 9007199254740993", got)
			}
			if _, err := w.Write([]byte(`{"key_alias":"example-key"}`)); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := litellm.New(server.URL, masterKey, nil)
	info, err := client.KeyInfo(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Fatal("key info returned no key")
	}
	if _, err := client.UpdateKey(t.Context(), virtualKey, litellm.UpdateKeyRequest{Metadata: &info.Metadata}); err != nil {
		t.Fatal(err)
	}
	if updates.Load() != 1 {
		t.Errorf("got %d updates, want 1", updates.Load())
	}
}
