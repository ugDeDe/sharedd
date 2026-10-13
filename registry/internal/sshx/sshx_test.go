package sshx

import "testing"

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
