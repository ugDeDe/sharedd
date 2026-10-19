package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type blockedKeepaliveConn struct {
	ssh.Conn
	netConn net.Conn
	started chan struct{}
}

func (c blockedKeepaliveConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	close(c.started)
	_, err := c.netConn.Write([]byte("keepalive"))
	return false, nil, err
}

func TestKeepaliveBlockedWrite(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancellation), func(t *testing.T) {
			conn, peer := net.Pipe() // No peer reader: the request write must block.
			defer conn.Close()
			defer peer.Close()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			started, done := make(chan struct{}), make(chan struct{})
			client := &ssh.Client{Conn: blockedKeepaliveConn{netConn: conn, started: started}}
			go func() {
				defer close(done)
				keepalive(ctx, client, time.Millisecond, 100*time.Millisecond, cancel)
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("keepalive request did not start")
			}
			want := ErrTransportLost
			if cancellation {
				want = context.Canceled
				cancel(want)
			}
			select {
			case <-done:
				if !errors.Is(context.Cause(ctx), want) {
					t.Fatalf("cause = %v, want %v", context.Cause(ctx), want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("blocked SendRequest goroutine did not exit")
			}
		})
	}
}

func TestRunKeepalive(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"session", "exec", "stdin", "wait", "silent-negative", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				cfg := &ssh.ServerConfig{NoClientAuth: true}
				cfg.AddHostKey(signer)
				server, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer server.Close()
				silentDone := make(chan struct{})
				requestsDone := make(chan struct{})
				go func() {
					defer close(requestsDone)
					count := 0
					for req := range requests {
						if stage == "cancel" {
							cancel() // Cancel while SendRequest awaits its reply.
						}
						if stage != "silent-negative" {
							continue // Blackhole: keep the socket open, never reply.
						}
						if req.Type != "keepalive@openssh.com" || !req.WantReply {
							return
						}
						if err := req.Reply(false, nil); err != nil {
							return
						}
						count++
						if count == 25 {
							close(silentDone)
						}
					}
				}()
				defer func() {
					_ = server.Close()
					<-requestsDone
				}()
				for incoming := range channels {
					if stage == "session" {
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					for req := range requests {
						if req.Type != "exec" || stage == "exec" {
							continue
						}
						if err := req.Reply(true, nil); err != nil {
							return
						}
						if stage == "silent-negative" {
							select {
							case <-silentDone:
							case <-ctx.Done():
								return
							}
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							_ = channel.Close()
						} else {
							_, _ = channel.Write([]byte("partial output"))
						}
					}
				}
			}()
			// This outer deadline is only a test safety net, not the liveness budget.
			runCtx, deadlineCancel := context.WithTimeout(ctx, 3*time.Second)
			defer deadlineCancel()
			stdin := ""
			if stage == "stdin" {
				stdin = strings.Repeat("x", 4<<20) // Exceeds the unread SSH channel window.
			}
			res, err := run(runCtx, Target{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port,
				Auth: Auth{Type: "password", Password: "test"}}, "true", time.Minute, stdin,
				10*time.Millisecond, 150*time.Millisecond)
			switch stage {
			case "silent-negative":
				if err != nil || res == nil || res.Code != 0 || res.Stdout != "" || res.Stderr != "" {
					t.Fatalf("silent command: result=%+v err=%v", res, err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("want cancellation, got %v", err)
				}
			default:
				if !errors.Is(err, ErrTransportLost) {
					t.Fatalf("want transport loss before safety deadline, got %v", err)
				}
				if stage != "session" && stage != "exec" && (res == nil || res.Stdout != "partial output") {
					t.Fatalf("lost partial output: %+v", res)
				}
			}
			cancel()
			select {
			case <-serverDone:
			case <-time.After(3 * time.Second):
				t.Fatal("SSH server/request goroutines did not exit")
			}
		})
	}
}

func TestRunSessionFailures(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"session", "exec", "running", "disconnect", "success", "exit"} {
		t.Run(stage, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				cfg := &ssh.ServerConfig{NoClientAuth: true}
				cfg.AddHostKey(signer)
				server, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if stage == "session" {
						continue // Deliberately leave channel-open unanswered.
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					for req := range requests {
						if req.Type != "exec" || stage == "exec" {
							continue
						}
						_ = req.Reply(true, nil)
						if stage == "disconnect" {
							return
						}
						if stage == "running" {
							_, _ = channel.Write([]byte("partial output"))
							continue
						}
						status := uint32(0)
						if stage == "exit" {
							status = 7
						}
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
						_ = channel.Close()
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			res, err := Run(ctx, Target{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port,
				Auth: Auth{Type: "password", Password: "test"}}, "true", 200*time.Millisecond, "")
			if stage == "success" || stage == "exit" {
				want := 0
				if stage == "exit" {
					want = 7
				}
				if err != nil || res == nil || res.Code != want {
					t.Fatalf("result = %+v, err = %v; want exit %d", res, err, want)
				}
			} else if err == nil {
				t.Fatal("broken or stalled SSH session must fail")
			} else if stage != "disconnect" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("want deadline error, got %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("step timeout did not interrupt SSH before outer deadline")
			}
			<-serverDone
		})
	}
}

func TestRunBoundsStalledHandshake(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelContext), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					accepted <- conn
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 50 * time.Millisecond
			if cancelContext {
				timeout = time.Minute
			}
			done := make(chan error, 1)
			go func() {
				_, err := Run(ctx, Target{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port,
					Auth: Auth{Type: "password", Password: "test"}}, "true", timeout, "")
				done <- err
			}()
			select {
			case conn := <-accepted:
				defer conn.Close()
			case <-time.After(3 * time.Second):
				t.Fatal("SSH did not connect")
			}
			if cancelContext {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("stalled handshake must fail")
				}
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("stalled handshake ignored timeout/cancellation")
			}
		})
	}
}

func TestAuthAndTargetEmpty(t *testing.T) {
	if !(Auth{}).Empty() {
		t.Fatal("zero auth must be empty")
	}
	if !(Auth{Type: "key"}).Empty() || !(Auth{Type: "password"}).Empty() {
		t.Fatal("auth without secret material must be empty")
	}
	if (Auth{Type: "key", Key: "k"}).Empty() {
		t.Fatal("key auth with material must not be empty")
	}
	if !(Target{Host: "1.2.3.4", User: "root"}).Empty() {
		t.Fatal("target without auth must be empty")
	}
	if (Target{Host: "1.2.3.4", User: "root", Auth: Auth{Type: "key", Key: "k"}}).Empty() {
		t.Fatal("complete target must not be empty")
	}
}

func TestClientConfigRejectsBadAuth(t *testing.T) {
	if _, err := clientConfig(Auth{Type: "password"}); err == nil {
		t.Fatal("empty password must error")
	}
	if _, err := clientConfig(Auth{Type: "key", Key: "not-a-key"}); err == nil {
		t.Fatal("bad key must error")
	}
	if _, err := clientConfig(Auth{Type: "weird", Key: "x"}); err == nil {
		t.Fatal("unknown auth type must error")
	}
}
