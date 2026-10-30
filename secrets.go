package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// secretStore хранит пароли и парольные фразы SSH в state\secrets.json.
// В Windows значения шифруются средствами системы (DPAPI) для текущего пользователя.
type secretStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string // ключ -> зашифрованное значение (base64)
}

func loadSecrets(stateDir string) *secretStore {
	s := &secretStore{path: filepath.Join(stateDir, "secrets.json"), m: map[string]string{}}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	return s
}

func secretKey(conn, kind string) string { return "conn/" + conn + "/" + kind }

func (s *secretStore) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[key]
	return ok
}

func (s *secretStore) Get(key string) (string, bool) {
	s.mu.Lock()
	enc, ok := s.m[key]
	s.mu.Unlock()
	if !ok {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	plain, err := unprotectSecret(raw)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

// Set сохраняет секрет; пустое значение удаляет его.
func (s *secretStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == "" {
		delete(s.m, key)
	} else {
		enc, err := protectSecret([]byte(value))
		if err != nil {
			return err
		}
		s.m[key] = base64.StdEncoding.EncodeToString(enc)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(s.m)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *secretStore) DeleteConn(conn string) {
	_ = s.Set(secretKey(conn, "password"), "")
	_ = s.Set(secretKey(conn, "passphrase"), "")
}
