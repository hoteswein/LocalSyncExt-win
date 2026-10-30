package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Connection — SSH/SFTP-подключение из раздела [Connection имя] конфигурации.
type Connection struct {
	Name     string
	Desc     string
	Host     string
	Port     int
	User     string
	Auth     string // key | password
	KeyFile  string
	HostKey  string // отпечаток SHA256:... сервера, которому доверяем
	Explicit []KV
	Err      string
}

// HostKeyError — сервер не подтверждён или его отпечаток изменился.
type HostKeyError struct {
	Fingerprint string
	Expected    string
}

func (e *HostKeyError) Error() string {
	if e.Expected == "" {
		return "отпечаток сервера не подтверждён (" + e.Fingerprint + "): проверьте и подтвердите его в настройках подключения"
	}
	return fmt.Sprintf("отпечаток сервера изменился (был %s, стал %s): возможна подмена сервера, подключение отклонено", e.Expected, e.Fingerprint)
}

type sftpFS struct {
	cfg     Connection
	secrets *secretStore

	mu   sync.Mutex
	sc   *ssh.Client
	cl   *sftp.Client
	done chan struct{}
}

func newSFTPFS(c Connection, secrets *secretStore) *sftpFS {
	return &sftpFS{cfg: c, secrets: secrets}
}

func (s *sftpFS) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if s.cfg.Auth == "password" {
		pw, ok := s.secrets.Get(secretKey(s.cfg.Name, "password"))
		if !ok {
			return nil, errors.New("пароль не задан (Настройки → Подключения)")
		}
		methods = append(methods, ssh.Password(pw))
		methods = append(methods, ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = pw
			}
			return ans, nil
		}))
		return methods, nil
	}
	if s.cfg.KeyFile == "" {
		return nil, errors.New("не указан файл ключа")
	}
	data, err := os.ReadFile(s.cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("файл ключа: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	var pme *ssh.PassphraseMissingError
	if errors.As(err, &pme) {
		pp, ok := s.secrets.Get(secretKey(s.cfg.Name, "passphrase"))
		if !ok {
			return nil, errors.New("ключ защищён парольной фразой: задайте её в настройках подключения")
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(pp))
	}
	if err != nil {
		return nil, fmt.Errorf("ключ не прочитан: %w", err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}

func (s *sftpFS) hostKeyCB(_ string, _ net.Addr, key ssh.PublicKey) error {
	fp := ssh.FingerprintSHA256(key)
	if s.cfg.HostKey == "" {
		return &HostKeyError{Fingerprint: fp}
	}
	if fp != s.cfg.HostKey {
		return &HostKeyError{Fingerprint: fp, Expected: s.cfg.HostKey}
	}
	return nil
}

func (s *sftpFS) addr() string {
	port := s.cfg.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(s.cfg.Host, strconv.Itoa(port))
}

func (s *sftpFS) connectLocked() error {
	auth, err := s.authMethods()
	if err != nil {
		return err
	}
	sc, err := ssh.Dial("tcp", s.addr(), &ssh.ClientConfig{
		User: s.cfg.User, Auth: auth, HostKeyCallback: s.hostKeyCB, Timeout: 15 * time.Second,
	})
	if err != nil {
		var hk *HostKeyError
		if errors.As(err, &hk) {
			return hk
		}
		return err
	}
	cl, err := sftp.NewClient(sc, sftp.UseConcurrentWrites(true), sftp.MaxConcurrentRequestsPerFile(16))
	if err != nil {
		sc.Close()
		return err
	}
	s.sc, s.cl, s.done = sc, cl, make(chan struct{})
	go keepAlive(sc, s.done)
	return nil
}

func keepAlive(sc *ssh.Client, done chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if _, _, err := sc.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				sc.Close()
				return
			}
		}
	}
}

func (s *sftpFS) closeLocked() {
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	if s.cl != nil {
		s.cl.Close()
		s.cl = nil
	}
	if s.sc != nil {
		s.sc.Close()
		s.sc = nil
	}
}

// Close закрывает соединение (оно откроется снова при следующем обращении).
func (s *sftpFS) Close() {
	s.mu.Lock()
	s.closeLocked()
	s.mu.Unlock()
}

func (s *sftpFS) client() (*sftp.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl == nil {
		if err := s.connectLocked(); err != nil {
			return nil, err
		}
	}
	return s.cl, nil
}

func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	m := err.Error()
	return strings.Contains(m, "connection lost") || strings.Contains(m, "closed") || strings.Contains(m, "broken pipe") || strings.Contains(m, "reset by peer")
}

// with выполняет операцию; при обрыве соединения закрывает его, а читающие операции повторяет один раз.
func (s *sftpFS) with(retry bool, fn func(c *sftp.Client) error) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	err = fn(c)
	if isConnErr(err) {
		s.mu.Lock()
		if s.cl == c {
			s.closeLocked()
		}
		s.mu.Unlock()
		if retry {
			if c, cerr := s.client(); cerr == nil {
				err = fn(c)
			}
		}
	}
	return err
}

// ---------- FileFS ----------

func (s *sftpFS) Local() bool         { return false }
func (s *sftpFS) CaseSensitive() bool { return true }
func (s *sftpFS) Join(e ...string) string {
	return path.Join(e...)
}
func (s *sftpFS) Dir(p string) string       { return path.Dir(p) }
func (s *sftpFS) Base(p string) string      { return path.Base(p) }
func (s *sftpFS) FromRel(rel string) string { return rel }
func (s *sftpFS) RelSlash(base, p string) (string, error) {
	base, p = path.Clean(base), path.Clean(p)
	if p == base {
		return ".", nil
	}
	pre := strings.TrimSuffix(base, "/") + "/"
	if !strings.HasPrefix(p, pre) {
		return "", fmt.Errorf("путь %s не внутри %s", p, base)
	}
	return p[len(pre):], nil
}

func (s *sftpFS) Lstat(p string) (fi fs.FileInfo, err error) {
	err = s.with(true, func(c *sftp.Client) (e error) { fi, e = c.Lstat(p); return })
	return
}

func (s *sftpFS) Stat(p string) (fi fs.FileInfo, err error) {
	err = s.with(true, func(c *sftp.Client) (e error) { fi, e = c.Stat(p); return })
	return
}

func (s *sftpFS) ReadDir(p string) (out []fs.DirEntry, err error) {
	err = s.with(true, func(c *sftp.Client) error {
		infos, e := c.ReadDir(p)
		out = out[:0]
		for _, fi := range infos {
			out = append(out, fs.FileInfoToDirEntry(fi))
		}
		return e
	})
	return
}

func (s *sftpFS) Open(p string) (f RFile, err error) {
	err = s.with(true, func(c *sftp.Client) (e error) {
		var sf *sftp.File
		sf, e = c.Open(p)
		if e == nil {
			f = sf
		}
		return
	})
	return
}

func (s *sftpFS) Create(p string, excl bool) (f WFile, err error) {
	flags := os.O_WRONLY | os.O_CREATE
	if excl {
		flags |= os.O_EXCL
	} else {
		flags |= os.O_TRUNC
	}
	err = s.with(false, func(c *sftp.Client) (e error) {
		var sf *sftp.File
		sf, e = c.OpenFile(p, flags)
		if e == nil {
			f = sftpWriter{sf}
		}
		return
	})
	if err != nil && excl {
		if _, serr := s.Lstat(p); serr == nil {
			err = fs.ErrExist
		}
	}
	return
}

// sftpWriter: Sync на серверах без fsync не должен считаться ошибкой.
type sftpWriter struct{ *sftp.File }

func (w sftpWriter) Sync() error {
	if err := w.File.Sync(); err != nil && !strings.Contains(err.Error(), "unsupported") && !strings.Contains(err.Error(), "not supported") {
		return nil // fsync — необязательное расширение
	}
	return nil
}

func (s *sftpFS) ReadFile(p string) ([]byte, error) {
	f, err := s.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (s *sftpFS) WriteFile(p string, data []byte) error {
	f, err := s.Create(p, false)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *sftpFS) Rename(a, b string) error {
	return s.with(false, func(c *sftp.Client) error {
		if _, ok := c.HasExtension("posix-rename@openssh.com"); ok {
			return c.PosixRename(a, b)
		}
		return c.Rename(a, b)
	})
}

func (s *sftpFS) Remove(p string) error {
	return s.with(false, func(c *sftp.Client) error { return c.Remove(p) })
}

func (s *sftpFS) MkdirAll(p string) error {
	return s.with(false, func(c *sftp.Client) error { return c.MkdirAll(p) })
}

func (s *sftpFS) Chtimes(p string, t time.Time) error {
	return s.with(false, func(c *sftp.Client) error { return c.Chtimes(p, t, t) })
}

func (s *sftpFS) AttrsOf(info fs.FileInfo) *Attrs {
	st, ok := info.Sys().(*sftp.FileStat)
	if !ok {
		return nil
	}
	return &Attrs{Mode: uint32(info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)), UID: st.UID, GID: st.GID, Owner: true}
}

func (s *sftpFS) HasAttrs() bool { return true }

func (s *sftpFS) SetAttrs(p string, a Attrs) error {
	return s.with(false, func(c *sftp.Client) error {
		if err := c.Chmod(p, fs.FileMode(a.Mode)); err != nil {
			return err
		}
		if a.Owner {
			return c.Chown(p, int(a.UID), int(a.GID))
		}
		return nil
	})
}

// ---------- менеджер подключений ----------

type connMgr struct {
	secrets *secretStore
	mu      sync.Mutex
	fs      map[string]*sftpFS
	sigs    map[string]string
}

func (c Connection) sig() string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s|%s", c.Name, c.Host, c.Port, c.User, c.Auth, c.KeyFile, c.HostKey)
}

func newConnMgr(stateDir string) *connMgr {
	return &connMgr{secrets: loadSecrets(stateDir), fs: map[string]*sftpFS{}, sigs: map[string]string{}}
}

func connKey(name string) string { return strings.ToLower(name) }

// Update применяет список подключений из конфигурации: изменённые пересоздаются, удалённые закрываются.
func (m *connMgr) Update(conns []*Connection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range conns {
		if c.Err != "" {
			continue
		}
		k := connKey(c.Name)
		seen[k] = true
		cc := *c
		cc.Explicit = nil
		if old, ok := m.sigs[k]; ok && old == cc.sig() {
			continue
		}
		if f := m.fs[k]; f != nil {
			f.Close()
		}
		m.sigs[k] = cc.sig()
		m.fs[k] = newSFTPFS(cc, m.secrets)
	}
	for k, f := range m.fs {
		if !seen[k] {
			f.Close()
			delete(m.fs, k)
			delete(m.sigs, k)
		}
	}
}

func (m *connMgr) sig(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sigs[connKey(name)]
}

func (m *connMgr) Get(name string) (FileFS, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.fs[connKey(name)]
	if f == nil {
		return nil, fmt.Errorf("подключение %q не найдено или содержит ошибку", name)
	}
	return f, nil
}

func (m *connMgr) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.fs {
		f.Close()
	}
}

// probeFingerprint получает отпечаток сервера, не отправляя ему никаких учётных данных.
func probeFingerprint(c Connection) (string, error) {
	var fp string
	port := c.Port
	if port == 0 {
		port = 22
	}
	_, err := ssh.Dial("tcp", net.JoinHostPort(c.Host, strconv.Itoa(port)), &ssh.ClientConfig{
		User: c.User,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp = ssh.FingerprintSHA256(key)
			return nil
		},
		Timeout: 15 * time.Second,
	})
	if fp != "" {
		return fp, nil // дальнейшая ошибка (нет способов входа) ожидаема
	}
	if err == nil {
		err = errors.New("сервер не вернул ключ")
	}
	return "", err
}
