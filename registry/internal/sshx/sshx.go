// Package sshx — минимальный SSH-клиент для выполнения shell-команд на VPS
// пользователя (порт mtplane/internal/sshx, адаптирован под registry:
// context-aware, без глобального состояния). Профиль установки — это
// упорядоченный список shell-команд; каждая выполняется отдельной сессией.
//
// Хост-ключ VPS не пинится (InsecureIgnoreHostKey, как в mtplane): VPS только
// что создан провайдером, known_hosts для него нет, а доступ идёт по
// сгенерированному нами ключу к нашему же серверу. Пининг добавим, если
// появится долгоживущий инвентарь хостов.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Auth — способ аутентификации: "key" (приватный ключ OpenSSH в Key) или
// "password" (пароль в Password). Ровно одно из полей.
type Auth struct {
	Type     string `json:"type"`
	Password string `json:"password"`
	Key      string `json:"key"`
}

// Empty — аутентификация не задана.
func (a Auth) Empty() bool {
	return a.Type == "" || (a.Type == "key" && strings.TrimSpace(a.Key) == "") ||
		(a.Type == "password" && a.Password == "")
}

// Target — хост/порт/пользователь + креды.
type Target struct {
	Host string
	Port int
	User string
	Auth Auth
}

// Empty — цель не готова к подключению.
func (t Target) Empty() bool {
	return strings.TrimSpace(t.Host) == "" || strings.TrimSpace(t.User) == "" || t.Auth.Empty()
}

// Result — итог одной команды.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// ErrTransportLost means the SSH peer stopped acknowledging keepalives.
var ErrTransportLost = errors.New("ssh_transport_lost")

const (
	keepaliveInterval = 20 * time.Second
	keepaliveTimeout  = 60 * time.Second
)

func clientConfig(auth Auth) (*ssh.ClientConfig, error) {
	cfg := &ssh.ClientConfig{
		Auth:            []ssh.AuthMethod{},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         20 * time.Second,
	}
	switch auth.Type {
	case "password":
		if auth.Password == "" {
			return nil, errors.New("ssh: empty password")
		}
		cfg.Auth = append(cfg.Auth, ssh.Password(auth.Password))
	case "key":
		signer, err := parseKey(auth.Key)
		if err != nil {
			return nil, err
		}
		cfg.Auth = append(cfg.Auth, ssh.PublicKeys(signer))
	default:
		return nil, errors.New("ssh: unknown auth type: " + auth.Type)
	}
	return cfg, nil
}

// ValidatePrivateKey — валидный ли это незашифрованный PEM/OpenSSH приватный
// ключ. Используется валидацией manual-серверов (пароль не требуется).
func ValidatePrivateKey(pemKey string) error {
	_, err := parseKey(pemKey)
	return err
}

func parseKey(pemKey string) (ssh.Signer, error) {
	pemKey = strings.TrimSpace(pemKey)
	if pemKey == "" {
		return nil, errors.New("ssh: empty private key")
	}
	signer, err := ssh.ParsePrivateKey([]byte(pemKey))
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, errors.New("ssh: ключ защищён паролем — не поддерживается")
		}
		return nil, err
	}
	return signer, nil
}

// Run выполняет одну команду на цели. ctx отменяет ожидание; timeout
// ограничивает один прогон (что наступит раньше). Ненулевой код выхода не
// ошибка: он в Result.Code, err != nil только при сбое транспорта/таймауте.
func Run(ctx context.Context, t Target, command string, timeout time.Duration, stdin string) (*Result, error) {
	return run(ctx, t, command, timeout, stdin, keepaliveInterval, keepaliveTimeout)
}

func run(ctx context.Context, t Target, command string, timeout time.Duration, stdin string, interval, liveness time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	if t.Port == 0 {
		t.Port = 22
	}
	if t.User == "" {
		t.User = "root"
	}
	cfg, err := clientConfig(t.Auth)
	if err != nil {
		return nil, err
	}
	cfg.User = t.User

	dialer := &net.Dialer{Timeout: cfg.Timeout}
	addr := net.JoinHostPort(t.Host, fmt.Sprintf("%d", t.Port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Closing the transport interrupts handshake, session setup and blocked writes.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if err := conn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		return nil, fmt.Errorf("ssh handshake deadline: %w", err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh handshake: %w", errors.Join(err, ctx.Err()))
	}
	defer func() { _ = sshConn.Close() }()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("ssh session deadline: %w", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = client.Close() }()
	keepaliveDone := make(chan struct{})
	go func() {
		defer close(keepaliveDone)
		keepalive(ctx, client, interval, liveness, cancelCause)
	}()
	defer func() {
		cancelCause(nil)
		_ = conn.Close() // Unblock an outstanding SendRequest before joining it.
		<-keepaliveDone
	}()

	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh session: %w", errors.Join(err, context.Cause(ctx)))
	}
	defer func() { _ = sess.Close() }()

	if stdin != "" {
		sess.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	// Run waits for output readers too, so buffers are safe to read on failure.
	err = sess.Run(command)
	res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() != nil {
		return res, fmt.Errorf("ssh: %w", context.Cause(ctx))
	}
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitStatus()
			return res, nil
		}
	}
	return res, err
}

func keepalive(ctx context.Context, client *ssh.Client, interval, timeout time.Duration, cancel context.CancelCauseFunc) {
	// The independent watchdog closes the transport via the existing context hook,
	// even when SendRequest is blocked writing or waiting for its reply.
	watchdog := time.AfterFunc(timeout, func() { cancel(ErrTransportLost) })
	defer watchdog.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				cancel(fmt.Errorf("%w: %v", ErrTransportLost, err))
				return
			}
			// SSH_REQUEST_FAILURE also proves that the peer is alive.
			watchdog.Reset(timeout)
		}
	}
}

// WaitSSH ждёт доступности SSH: до attempts попыток с паузой delay.
func WaitSSH(ctx context.Context, t Target, attempts int, delay time.Duration) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := Run(ctx, t, "echo __ok__", 20*time.Second, "")
		if err == nil && strings.Contains(res.Stdout, "__ok__") {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("ssh: no __ok__ in output")
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("ssh: не отвечает")
	}
	return lastErr
}
