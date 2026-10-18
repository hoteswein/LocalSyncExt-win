package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pbkdf2Iter = 150000

// pbkdf2SHA256 — PBKDF2-HMAC-SHA256 (RFC 8018) на стандартной библиотеке.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	var buf [4]byte
	dk := make([]byte, 0, blocks*hLen)
	u := make([]byte, hLen)
	for b := 1; b <= blocks; b++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf[:], uint32(b))
		prf.Write(buf[:])
		dk = prf.Sum(dk)
		t := dk[len(dk)-hLen:]
		u = append(u[:0], t...)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := range u {
				t[x] ^= u[x]
			}
		}
	}
	return dk[:keyLen]
}

func hashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	h := pbkdf2SHA256([]byte(password), salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h))
}

func checkPassword(stored, password string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1000 || iter > 5000000 {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(p[2])
	want, e2 := base64.RawStdEncoding.DecodeString(p[3])
	if e1 != nil || e2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

type webCreds struct {
	User   string `json:"user"`
	Hash   string `json:"hash"`
	Secret string `json:"secret"`
}

// credStore хранит логин, хэш пароля и секрет подписи cookie в state\web.json.
type credStore struct {
	mu   sync.Mutex
	path string
	c    webCreds
}

func loadCreds(stateDir string) *credStore {
	s := &credStore{path: filepath.Join(stateDir, "web.json")}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.c)
	}
	return s
}

func (s *credStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(s.c)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *credStore) HasPassword() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.User != "" && s.c.Hash != ""
}

func (s *credStore) User() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.User
}

// Set задаёт логин и пароль; секрет подписи меняется, поэтому все прежние входы перестают действовать.
func (s *credStore) Set(user, password string) error {
	user = strings.TrimSpace(user)
	if user == "" || strings.ContainsAny(user, "|\r\n") || len(user) > 64 {
		return errors.New("логин пустой или содержит недопустимые символы")
	}
	if len(password) < 6 {
		return errors.New("пароль должен быть не короче 6 символов")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c = webCreds{User: user, Hash: hashPassword(password), Secret: newSecret()}
	return s.saveLocked()
}

func (s *credStore) Verify(user, password string) bool {
	s.mu.Lock()
	cu, ch := s.c.User, s.c.Hash
	s.mu.Unlock()
	if cu == "" || ch == "" {
		return false
	}
	ok := checkPassword(ch, password) // проверяем всегда, чтобы время не выдавало верный логин
	return ok && subtle.ConstantTimeCompare([]byte(user), []byte(cu)) == 1
}

func (s *credStore) sign(payload string) string {
	if s.c.Secret == "" {
		s.c.Secret = newSecret()
		_ = s.saveLocked()
	}
	m := hmac.New(sha256.New, []byte(s.c.Secret))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Token выдаёт подписанный токен для cookie.
func (s *credStore) Token(user string, ttl time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := user + "|" + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.sign(payload)
}

// CheckToken проверяет подпись и срок действия.
func (s *credStore) CheckToken(tok string) (string, bool) {
	i := strings.IndexByte(tok, '.')
	if i < 0 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return "", false
	}
	payload := string(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.User == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(s.sign(payload)), []byte(tok[i+1:])) != 1 {
		return "", false
	}
	j := strings.LastIndexByte(payload, '|')
	if j < 0 {
		return "", false
	}
	exp, err := strconv.ParseInt(payload[j+1:], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	user := payload[:j]
	if subtle.ConstantTimeCompare([]byte(user), []byte(s.c.User)) != 1 {
		return "", false
	}
	return user, true
}

// ---------- ограничение подбора пароля ----------

type attempt struct {
	fails int
	until time.Time
}

type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*attempt
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{m: map[string]*attempt{}} }

// Wait возвращает, сколько ещё ждать до следующей попытки с этого адреса.
func (l *loginLimiter) Wait(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.m[ip]; a != nil && time.Now().Before(a.until) {
		return time.Until(a.until)
	}
	return 0
}

func (l *loginLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[ip]
	if a == nil {
		a = &attempt{}
		l.m[ip] = a
	}
	a.fails++
	if a.fails >= 5 {
		d := 30 * time.Second << uint(a.fails-5)
		if d > 15*time.Minute || d <= 0 {
			d = 15 * time.Minute
		}
		a.until = time.Now().Add(d)
	}
	if len(l.m) > 10000 { // не растём бесконечно
		l.m = map[string]*attempt{ip: a}
	}
}

func (l *loginLimiter) OK(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}
