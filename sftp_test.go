//go:build !windows

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// testSSHD — встроенный SSH/SFTP-сервер для проверок (работает с настоящими файлами этого компьютера).
type testSSHD struct {
	ln    net.Listener
	fp    string
	port  int
	mu    sync.Mutex
	conns []net.Conn
}

func startSSHD(t *testing.T, user, pass string) *testSSHD {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == user && string(p) == pass {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHD{ln: ln, fp: ssh.FingerprintSHA256(signer.PublicKey()), port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, nc)
			s.mu.Unlock()
			go serveSSH(nc, cfg)
		}
	}()
	t.Cleanup(s.Stop)
	return s
}

func serveSSH(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range creqs {
				ok := req.Type == "subsystem" && len(req.Payload) >= 8 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					srv, err := sftp.NewServer(ch)
					if err == nil {
						_ = srv.Serve()
					}
					ch.Close()
					return
				}
			}
		}()
	}
	sc.Close()
}

func (s *testSSHD) Stop() {
	s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

func sftpEnv(t *testing.T, sshd *testSSHD, hostkey string, rules string) (*Engine, *Config, string) {
	t.Helper()
	root := t.TempDir()
	ini := fmt.Sprintf("[Connection srv]\nhost=127.0.0.1\nport=%d\nuser=tester\nauth=password\nhostkey=%s\n\n", sshd.port, hostkey) + rules
	ini = strings.ReplaceAll(ini, "{R}", filepath.ToSlash(root))
	p := filepath.Join(root, "t.ini")
	os.WriteFile(p, []byte(ini), 0o644)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range cfg.Rules {
		if r.Err != "" {
			t.Fatalf("правило %s: %s", r.Name, r.Err)
		}
	}
	en := NewEngine(cfg, filepath.Join(root, "state"), false, io.Discard)
	if err := en.cm.secrets.Set(secretKey("srv", "password"), "pw123456"); err != nil {
		t.Fatal(err)
	}
	return en, cfg, root
}

func TestSFTPSync(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	en, cfg, root := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
	r := cfg.Rules[0]
	if r.kind(1) != "remote" || r.Endpoints[1].Conn != "srv" {
		t.Fatalf("SFTP-путь должен быть сетевым: %q %q", r.kind(1), r.Endpoints[1].Conn)
	}
	local, remote := filepath.Join(root, "local"), filepath.Join(root, "remote")
	wr(t, filepath.Join(local, "a.txt"), "alpha", time.Minute)
	wr(t, filepath.Join(local, "sub", "b.txt"), "beta", time.Minute)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(remote, "a.txt")) != "alpha" || rd(t, filepath.Join(remote, "sub", "b.txt")) != "beta" {
		t.Fatal("файлы не скопированы на SFTP")
	}
	fi1, _ := os.Stat(filepath.Join(local, "a.txt"))
	fi2, _ := os.Stat(filepath.Join(remote, "a.txt"))
	if d := fi1.ModTime().Sub(fi2.ModTime()); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("время изменения не сохранилось: %v", d)
	}
	// изменение на сервере доходит до локальной папки
	wr(t, filepath.Join(remote, "a.txt"), "alpha-new", 10*time.Second)
	// удаление локально -> корзина на сервере
	os.Remove(filepath.Join(local, "sub", "b.txt"))
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(local, "a.txt")) != "alpha-new" {
		t.Fatal("изменение на сервере не дошло до локальной папки")
	}
	if exists(filepath.Join(remote, "sub", "b.txt")) {
		t.Fatal("удаление не дошло до сервера")
	}
	ents, _ := os.ReadDir(filepath.Join(remote, trashDir, "sub"))
	if len(ents) != 1 {
		t.Fatalf("удалённый файл должен лежать в корзине на сервере: %v", ents)
	}
	// новая версия файла: старая остаётся в корзине сервера, повторный проход ничего не меняет
	wr(t, filepath.Join(local, "a.txt"), "alpha-3", 5*time.Second)
	en.RunOnce()
	if rd(t, filepath.Join(remote, "a.txt")) != "alpha-3" {
		t.Fatal("обновление не дошло до сервера")
	}
	c0, _ := readCounters(en, r)
	en.RunOnce()
	if c1, _ := readCounters(en, r); c1 != c0 {
		t.Fatal("повторный проход не должен ничего копировать")
	}
}

func TestSFTPHostKeyVerification(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	for name, hk := range map[string]string{"не подтверждён": "", "изменился": "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		en, _, root := sftpEnv(t, sshd, hk, "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
		wr(t, filepath.Join(root, "local", "x.txt"), "x", time.Minute)
		if en.RunOnce() {
			t.Fatalf("при отпечатке %q подключение должно отклоняться", hk)
		}
		var found bool
		for _, e := range en.Events() {
			if e.Level == "ERROR" && strings.Contains(e.Msg, name) {
				found = true
			}
		}
		if !found {
			t.Fatalf("в журнале нет сообщения про %q: %+v", name, en.Events())
		}
		if exists(filepath.Join(root, "remote", "x.txt")) {
			t.Fatal("к недоверенному серверу ничего отправляться не должно")
		}
	}
	fp, err := probeFingerprint(Connection{Host: "127.0.0.1", Port: sshd.port, User: "x"})
	if err != nil || fp != sshd.fp {
		t.Fatalf("отпечаток сервера: %q %v", fp, err)
	}
}

func TestSFTPWrongPassword(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	en, _, root := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
	en.cm.secrets.Set(secretKey("srv", "password"), "неверный")
	wr(t, filepath.Join(root, "local", "x.txt"), "x", time.Minute)
	if en.RunOnce() {
		t.Fatal("неверный пароль должен давать ошибку")
	}
}

func TestSFTPPermissionsPreserved(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("для смены владельца нужен root")
	}
	sshd := startSSHD(t, "tester", "pw123456")
	en, _, root := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\nconflict=auto\n{R}/local\nsftp://srv{R}/remote\n[R2]\nscan=0\nwatch=0\nsftp://srv{R}/dirA\nsftp://srv{R}/dirB\n")
	local, remote := filepath.Join(root, "local"), filepath.Join(root, "remote")
	// на сервере исполняемый файл с владельцем 1234
	wr(t, filepath.Join(remote, "run.sh"), "#!/bin/sh\necho v1\n", time.Hour)
	os.Chmod(filepath.Join(remote, "run.sh"), 0o755)
	os.Chown(filepath.Join(remote, "run.sh"), 1234, 4321)
	en.RunOnce()
	if rd(t, filepath.Join(local, "run.sh")) != "#!/bin/sh\necho v1\n" {
		t.Fatal("файл не пришёл")
	}
	// приходит обновление с локальной стороны — права и владелец на сервере должны остаться
	wr(t, filepath.Join(local, "run.sh"), "#!/bin/sh\necho version-two\n", 30*time.Second)
	en.RunOnce()
	if rd(t, filepath.Join(remote, "run.sh")) != "#!/bin/sh\necho version-two\n" {
		t.Fatal("обновление не дошло до сервера")
	}
	fi, _ := os.Stat(filepath.Join(remote, "run.sh"))
	st := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != 0o755 || st.Uid != 1234 || st.Gid != 4321 {
		t.Fatalf("права и владелец потеряны: %v uid=%d gid=%d", fi.Mode().Perm(), st.Uid, st.Gid)
	}
	// новый файл между двумя SFTP-путями получает права источника
	dirA, dirB := filepath.Join(root, "dirA"), filepath.Join(root, "dirB")
	wr(t, filepath.Join(dirA, "tool"), "bin", time.Hour)
	os.Chmod(filepath.Join(dirA, "tool"), 0o750)
	os.Chown(filepath.Join(dirA, "tool"), 77, 88)
	en.RunOnce()
	fb, err := os.Stat(filepath.Join(dirB, "tool"))
	if err != nil {
		t.Fatal("файл не скопирован между SFTP-путями")
	}
	sb := fb.Sys().(*syscall.Stat_t)
	if fb.Mode().Perm() != 0o750 || sb.Uid != 77 || sb.Gid != 88 {
		t.Fatalf("новый файл должен получить права источника: %v %d:%d", fb.Mode().Perm(), sb.Uid, sb.Gid)
	}
}

func TestSFTPUnreachableDoesNotDelete(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	en, _, root := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
	local := filepath.Join(root, "local")
	for i := 0; i < 8; i++ {
		wr(t, filepath.Join(local, fmt.Sprintf("f%d.txt", i)), "data", time.Minute)
	}
	if !en.RunOnce() {
		t.Fatal("ошибка первого прохода")
	}
	sshd.Stop() // сервер пропал
	if en.RunOnce() {
		t.Fatal("при недоступном сервере проход должен завершаться ошибкой")
	}
	if !exists(filepath.Join(local, "f0.txt")) || !exists(filepath.Join(root, "remote", "f0.txt")) {
		t.Fatal("при потере связи файлы не должны удаляться")
	}
}

func TestWantAttrsHint(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	_, cfg, _ := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
	r := cfg.Rules[0]
	r.Endpoints[0].FS = theLocalFS
	r.Endpoints[1].FS = newSFTPFS(*cfg.Conns[0], loadSecrets(t.TempDir()))
	hint := &Attrs{Mode: 0o700, UID: 5, GID: 6, Owner: true}
	if got := wantAttrs(r, 1, "/nonexistent/file", FInfo{}, hint); got != hint {
		t.Fatalf("без заменяемого файла и без прав источника берём запомненные: %+v", got)
	}
	src := &Attrs{Mode: 0o644}
	if got := wantAttrs(r, 1, "/nonexistent/file", FInfo{Attr: src}, hint); got != src {
		t.Fatal("права источника важнее запомненных")
	}
}

func TestSecretsStore(t *testing.T) {
	d := t.TempDir()
	s := loadSecrets(d)
	k := secretKey("n", "password")
	if _, ok := s.Get(k); ok || s.Has(k) {
		t.Fatal("пусто")
	}
	if err := s.Set(k, "п@роль"); err != nil {
		t.Fatal(err)
	}
	s2 := loadSecrets(d)
	if v, ok := s2.Get(k); !ok || v != "п@роль" {
		t.Fatalf("секрет не сохранился: %q", v)
	}
	raw, _ := os.ReadFile(filepath.Join(d, "secrets.json"))
	_ = raw
	s2.DeleteConn("n")
	if s2.Has(k) {
		t.Fatal("секрет должен удаляться")
	}
}

func TestWebConnectionsAPI(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	en, _, root := sftpEnv(t, sshd, "", "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
	ini := filepath.Join(root, "t.ini")
	ws := NewWebServer(en, ini, filepath.Join(root, "t.log"), filepath.Join(root, "state"), "v-test", func() {})
	srv := newTestHTTP(t, ws)
	c := srv.Client()

	var cfg apiConfig
	code, b := do(t, c, "GET", srv.URL+"/api/config", nil, nil)
	if code != 200 || json.Unmarshal(b, &cfg) != nil || len(cfg.Connections) != 1 || !cfg.Connections[0].HasPassword || cfg.Connections[0].HostKey != "" {
		t.Fatalf("подключения в конфиге: %d %s", code, b)
	}
	if strings.Contains(string(b), "pw123456") {
		t.Fatal("пароль не должен отдаваться через API")
	}
	// отпечаток без отправки пароля
	var pr struct {
		Fingerprint, Current string
		Trusted              bool
	}
	code, b = do(t, c, "POST", srv.URL+"/api/conn/probe", map[string]string{"name": "srv"}, nil)
	json.Unmarshal(b, &pr)
	if code != 200 || pr.Fingerprint != sshd.fp || pr.Trusted {
		t.Fatalf("probe: %d %s", code, b)
	}
	// клиент без поля подключений не должен стирать подключения
	noConn := cfg
	noConn.Connections = nil
	if code, b := do(t, c, "POST", srv.URL+"/api/config", noConn, nil); code != 200 {
		t.Fatalf("сохранение без подключений: %d %s", code, b)
	}
	if kept, _ := os.ReadFile(ini); !strings.Contains(string(kept), "[Connection srv]") {
		t.Fatal("подключение потеряно при сохранении без поля connections")
	}
	_, b = do(t, c, "GET", srv.URL+"/api/config", nil, nil)
	json.Unmarshal(b, &cfg)
	// пока отпечаток не подтверждён, вход не выполняется
	code, b = do(t, c, "POST", srv.URL+"/api/conn/test", map[string]string{"name": "srv"}, nil)
	if code != 502 || !strings.Contains(string(b), sshd.fp) {
		t.Fatalf("test без доверия: %d %s", code, b)
	}
	// доверяем отпечатку и сохраняем
	cfg.Connections[0].HostKey = pr.Fingerprint
	code, b = do(t, c, "POST", srv.URL+"/api/config", cfg, nil)
	if code != 200 {
		t.Fatalf("сохранение: %d %s", code, b)
	}
	saved, _ := os.ReadFile(ini)
	if !strings.Contains(string(saved), "hostkey="+sshd.fp) || !strings.Contains(string(saved), "[Connection srv]") {
		t.Fatalf("ini после сохранения:\n%s", saved)
	}
	code, b = do(t, c, "POST", srv.URL+"/api/conn/test", map[string]string{"name": "srv"}, nil)
	if code != 200 || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("test с доверием: %d %s", code, b)
	}
	// обзор удалённых папок
	os.MkdirAll(filepath.Join(root, "remote", "docs"), 0o755)
	os.WriteFile(filepath.Join(root, "remote", "a.txt"), []byte("12"), 0o644)
	var fr fsResp
	code, b = do(t, c, "GET", srv.URL+"/api/fs?conn=srv&path="+filepath.ToSlash(filepath.Join(root, "remote")), nil, nil)
	json.Unmarshal(b, &fr)
	if code != 200 || len(fr.Entries) != 2 || !fr.Entries[0].Dir || fr.Entries[0].Name != "docs" || fr.Entries[1].Size != 2 || fr.Sep != "/" {
		t.Fatalf("удалённый обзор: %d %+v", code, fr)
	}
	if code, _ := do(t, c, "GET", srv.URL+"/api/fs?conn=нет&path=/", nil, nil); code != 400 {
		t.Fatal("неизвестное подключение должно давать 400")
	}
	// смена пароля через API: с неверным вход не проходит
	if code, _ := do(t, c, "POST", srv.URL+"/api/secret", map[string]string{"conn": "srv", "kind": "password", "value": "другой"}, nil); code != 200 {
		t.Fatal("secret")
	}
	if code, _ := do(t, c, "POST", srv.URL+"/api/conn/test", map[string]string{"name": "srv"}, nil); code != 502 {
		t.Fatal("с неверным паролем проверка должна падать")
	}
	if code, _ := do(t, c, "POST", srv.URL+"/api/secret", map[string]string{"conn": "нет", "kind": "password", "value": "x"}, nil); code != 404 {
		t.Fatal("секрет для несуществующего подключения")
	}
	// добавление подключения с ошибкой не сохраняется; удаление чистит секрет
	_, b = do(t, c, "GET", srv.URL+"/api/config", nil, nil)
	json.Unmarshal(b, &cfg)
	bad := cfg
	bad.Connections = append(append([]apiConn(nil), cfg.Connections...), apiConn{Name: "nohost", User: "u", Auth: "password"})
	if code, _ := do(t, c, "POST", srv.URL+"/api/config", bad, nil); code != 400 {
		t.Fatal("подключение без host должно блокировать сохранение")
	}
	_, b = do(t, c, "GET", srv.URL+"/api/config", nil, nil)
	json.Unmarshal(b, &cfg)
	cfg.Rules = nil // правила ссылаются на подключение — убираем, потом удаляем и его
	cfg.Connections = []apiConn{}
	if code, b := do(t, c, "POST", srv.URL+"/api/config", cfg, nil); code != 200 {
		t.Fatalf("удаление: %d %s", code, b)
	}
	if en.cm.secrets.Has(secretKey("srv", "password")) {
		t.Fatal("секрет удалённого подключения должен удаляться")
	}
}

func newTestHTTP(t *testing.T, ws *WebServer) *httptest.Server {
	s := httptest.NewServer(ws.handler(true))
	t.Cleanup(s.Close)
	return s
}
