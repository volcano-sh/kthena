/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModels(t *testing.T) {
	s := &server{model: "test-model"}
	srv := httptest.NewServer(http.HandlerFunc(s.handleModels))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "test-model" {
		t.Fatalf("unexpected body: %+v", out)
	}
}

func TestCompletionsNonStream(t *testing.T) {
	s := &server{model: "m"}
	srv := httptest.NewServer(http.HandlerFunc(s.handleCompletions))
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"model":"m","max_tokens":4,"stream":false}`)
	res, err := http.Post(srv.URL+"/v1/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var out struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Text != "tttt" {
		t.Fatalf("got %q", out.Choices[0].Text)
	}
}

func TestCompletionsStreamHasDONE(t *testing.T) {
	s := &server{model: "m"}
	srv := httptest.NewServer(http.HandlerFunc(s.handleCompletions))
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"model":"m","max_tokens":2,"stream":true}`)
	res, err := http.Post(srv.URL+"/v1/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "data: [DONE]") {
		t.Fatalf("missing [DONE]: %q", string(b))
	}
}
