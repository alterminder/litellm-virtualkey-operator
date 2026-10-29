package controller

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"

	"github.com/alterminder/litellm-virtualkey-operator/internal/litellm"
)

const (
	generatePath = "/key/generate"
	updatePath   = "/key/update"
	deletePath   = "/key/delete"
	infoPath     = "/v2/key/info"
)

// fakeLiteLLM models the v1.93.2 contracts used by option A, including
// alias uniqueness, metadata replacement, and info responses without tokens.
type fakeLiteLLM struct {
	mu          sync.Mutex
	server      *httptest.Server
	keys        map[string]litellm.KeyInfo
	calls       map[string]int
	history     []map[string]litellm.KeyInfo
	master      string
	failPath    string
	failCall    int
	failureBody string
}

func newFakeLiteLLM(master string) *fakeLiteLLM {
	f := &fakeLiteLLM{keys: map[string]litellm.KeyInfo{}, calls: map[string]int{}, master: master}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	return f
}

func (f *fakeLiteLLM) seed(value string, info litellm.KeyInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[litellm.TokenHash(value)] = info
}

func (f *fakeLiteLLM) state() map[string]litellm.KeyInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot()
}

func (f *fakeLiteLLM) snapshot() map[string]litellm.KeyInfo {
	state := make(map[string]litellm.KeyInfo, len(f.keys))
	maps.Copy(state, f.keys)
	return state
}

func (f *fakeLiteLLM) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = map[string]int{}
	f.history = nil
}

func (f *fakeLiteLLM) counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]int{generatePath: f.calls[generatePath], updatePath: f.calls[updatePath], deletePath: f.calls[deletePath]}
}

func (f *fakeLiteLLM) fail(path string, call int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPath, f.failCall, f.failureBody = path, call, body
}

func (f *fakeLiteLLM) aliasExists(alias, except string) bool {
	for hash, info := range f.keys {
		if hash != except && info.KeyAlias == alias {
			return true
		}
	}
	return false
}

func (f *fakeLiteLLM) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	f.calls[r.URL.Path]++
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+f.master {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == f.failPath && f.calls[r.URL.Path] == f.failCall {
		http.Error(w, f.failureBody, http.StatusInternalServerError)
		return
	}
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var response any
	switch r.URL.Path {
	case infoPath:
		var query litellm.DeleteKeysRequest
		if decoder.Decode(&query) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		infos := []litellm.KeyInfo{}
		for hash, info := range f.keys {
			selected := slices.Contains(query.KeyAliases, info.KeyAlias) || selectsHash(query.Keys, hash)
			if selected {
				info.TokenHash = ""
				infos = append(infos, info)
			}
		}
		response = map[string]any{"info": infos}
	case generatePath:
		var request litellm.GenerateKeyRequest
		if decoder.Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(request.Key, "sk-") || f.aliasExists(request.KeyAlias, "") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		metadata := map[string]any{}
		for key, value := range request.Metadata {
			metadata[key] = value
		}
		info := litellm.KeyInfo{KeyAlias: request.KeyAlias, Models: request.Models, UserID: request.UserID, Metadata: metadata}
		f.keys[litellm.TokenHash(request.Key)] = info
		response = litellm.GeneratedKey{KeyInfo: info, Key: request.Key}
	case updatePath:
		var request struct {
			litellm.UpdateKeyRequest
			Key string `json:"key"`
		}
		if decoder.Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hash := request.Key
		if strings.HasPrefix(hash, "sk-") {
			hash = litellm.TokenHash(hash)
		}
		info, ok := f.keys[hash]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if request.KeyAlias != nil {
			if f.aliasExists(*request.KeyAlias, hash) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			info.KeyAlias = *request.KeyAlias
		}
		if request.Models != nil {
			info.Models = *request.Models
		}
		if request.UserID != nil {
			info.UserID = *request.UserID
		}
		if request.Metadata != nil {
			info.Metadata = *request.Metadata
		}
		f.keys[hash] = info
		response = info
	case deletePath:
		var query litellm.DeleteKeysRequest
		if decoder.Decode(&query) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		deleted := []string{}
		for hash, info := range f.keys {
			if slices.Contains(query.KeyAliases, info.KeyAlias) {
				delete(f.keys, hash)
				deleted = append(deleted, info.KeyAlias)
			}
		}
		if len(deleted) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		response = litellm.DeleteKeysResponse{DeletedKeys: deleted}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Path != infoPath {
		f.history = append(f.history, f.snapshot())
	}
	_ = json.NewEncoder(w).Encode(response)
}

func selectsHash(keys []string, hash string) bool {
	for _, key := range keys {
		if strings.HasPrefix(key, "sk-") {
			key = litellm.TokenHash(key)
		}
		if key == hash {
			return true
		}
	}
	return false
}

func (f *fakeLiteLLM) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, count := range f.calls {
		total += count
	}
	return total
}
