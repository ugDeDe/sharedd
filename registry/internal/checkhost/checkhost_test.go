package checkhost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientNodesStartResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "need json", http.StatusNotAcceptable)
			return
		}
		switch r.URL.Path {
		case "/nodes/hosts":
			_, _ = w.Write([]byte(`{"nodes":{"ru1.node.check-host.net":{"location":["ru","Russia","Moscow"]},"de1.node.check-host.net":{"location":["de","Germany","Nuremberg"]}}}`))
		case "/check-tcp":
			q := r.URL.Query()
			if q.Get("host") != "1.2.3.4:22" || strings.Join(q["node"], ",") != "ru1.node.check-host.net,de1.node.check-host.net" {
				http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"ok":1,"request_id":"abc"}`))
		case "/check-result/abc":
			_, _ = w.Write([]byte(`{"ru1.node.check-host.net":[{"error":"Connection timed out"}],"de1.node.check-host.net":[{"time":0.05,"address":"1.2.3.4"}],"x.node":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	nodes, err := c.Nodes(context.Background())
	if err != nil || len(nodes) != 2 || nodes[1].Country != "ru" {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}
	id, err := c.StartTCP(context.Background(), "1.2.3.4", 22, []string{"ru1.node.check-host.net", "de1.node.check-host.net"})
	if err != nil || id != "abc" {
		t.Fatalf("start: %q %v", id, err)
	}
	res, err := c.Result(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r := res["ru1.node.check-host.net"]; r.OK || r.Error != "Connection timed out" {
		t.Fatalf("ru1: %+v", r)
	}
	if r := res["de1.node.check-host.net"]; !r.OK {
		t.Fatalf("de1: %+v", r)
	}
	if r := res["x.node"]; !r.Pending {
		t.Fatalf("null must be pending: %+v", r)
	}
}
