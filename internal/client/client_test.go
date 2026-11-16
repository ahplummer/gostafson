package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatSendsRequestAndParsesReply(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer k" {
			t.Errorf("Authorization = %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, `{
			"choices":[{"message":{"role":"assistant","content":"391","reasoning":"17*23"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1/", "k")
	reply, err := c.Chat(context.Background(), Request{
		Model:           "m",
		Messages:        []Message{{Role: "user", Content: "17*23"}},
		ReasoningEffort: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Message.Content != "391" || reply.Reasoning != "17*23" || reply.Usage.TotalTokens != 7 {
		t.Errorf("reply = %+v", reply)
	}
	if got["reasoning_effort"] != "none" || got["stream"] != false || got["model"] != "m" {
		t.Errorf("request = %v", got)
	}
	if _, ok := got["tools"]; ok {
		t.Error("tools should be omitted when empty")
	}
}

func TestChatParsesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"",
			"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]},
			"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	reply, err := New(srv.URL, "").Chat(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Message.ToolCalls) != 1 || reply.Message.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("tool calls = %+v", reply.Message.ToolCalls)
	}
}

func TestPing(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"data":[]}`)
	}))
	defer ok.Close()
	if err := New(ok.URL+"/v1", "").Ping(context.Background()); err != nil {
		t.Errorf("ping ok server: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer bad.Close()
	if err := New(bad.URL, "").Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("ping 401 server: %v", err)
	}

	bad.Close()
	if err := New(bad.URL, "").Ping(context.Background()); err == nil {
		t.Error("ping closed server: expected error")
	}
}

func TestChatReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").Chat(context.Background(), Request{Model: "missing"})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("err = %v", err)
	}
}

func TestChatParsesSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"news",
			"annotations":[
				{"type":"url_citation","url_citation":{"url":"https://a.example/1","title":"A"}},
				{"type":"url_citation","url_citation":{"url":"https://a.example/1","title":"A again"}},
				{"type":"other"}]},
			"finish_reason":"stop"}],
			"citations":["https://b.example/2","https://a.example/1"]}`)
	}))
	defer srv.Close()

	reply, err := New(srv.URL, "").Chat(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Source{{Title: "A", URL: "https://a.example/1"}, {URL: "https://b.example/2"}}
	if fmt.Sprint(reply.Sources) != fmt.Sprint(want) {
		t.Errorf("sources = %+v, want %+v", reply.Sources, want)
	}
}
