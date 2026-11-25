package sot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestCarriesTokenAndDecodesAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/api/federation/inbox/pull" {
			t.Errorf("unexpected request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var in map[string][]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["session_ids"][0] != "s1" {
			t.Errorf("session_ids = %v", in)
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"m1","from_user":"bob","body":"hi","to_session_id":"s1","reply_to":null}]}`))
	}))
	defer srv.Close()
	msgs, err := New(srv.URL, "tok").Pull(context.Background(), []string{"s1"})
	if err != nil || len(msgs) != 1 || msgs[0].FromUser != "bob" || msgs[0].Body != "hi" {
		t.Fatalf("Pull = %+v, %v", msgs, err)
	}
}

func TestServerErrorTextIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"No such session"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "t").Send(context.Background(), Outgoing{})
	if err == nil || !strings.Contains(err.Error(), "No such session") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerWithoutFederationIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`<h1>Not Found</h1>`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "t").WhoAmI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not support federation") {
		t.Fatalf("err = %v", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "t").WhoAmI(context.Background()); err == nil || hit {
		t.Fatalf("err = %v, hit = %v", err, hit)
	}
}

func TestVisibleToIsAllOrList(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	_ = c.Register(context.Background(), Registration{SessionID: "s", Everyone: true})
	if got["visible_to"] != "all" {
		t.Errorf("visible_to = %v", got["visible_to"])
	}
	_ = c.Register(context.Background(), Registration{SessionID: "s", VisibleTo: []string{"bob"}})
	if l, ok := got["visible_to"].([]any); !ok || len(l) != 1 || l[0] != "bob" {
		t.Errorf("visible_to = %v", got["visible_to"])
	}
}

func TestDeleteEscapesSessionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/federation/sessions/a%2Fb" {
			t.Errorf("path = %s", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := New(srv.URL, "t").Unregister(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
}
