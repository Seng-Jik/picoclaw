package integrationtools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestImageGenerateUsesModelProxy(t *testing.T) {
	var proxyCalled atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled.Store(true)
		if r.URL.String() != "http://image-model.invalid/v1/images/generations" {
			t.Errorf("request URL = %q", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = fmt.Fprintf(
			w,
			`{"data":[{"b64_json":%q}]}`,
			base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n")),
		)
	}))
	defer proxy.Close()

	cfg := config.DefaultConfig()
	cfg.Tools.ImageGenerate.ModelName = "image-model"
	cfg.ModelList = []*config.ModelConfig{{
		ModelName: "image-model",
		Model:     "openai/gpt-image-1",
		APIBase:   "http://image-model.invalid/v1",
		Proxy:     proxy.URL,
		APIKeys:   config.SimpleSecureStrings("key"),
	}}
	store := media.NewFileMediaStore()
	defer store.ReleaseAll("tool:image_generate:test:chat")
	tool := NewImageGenerateTool(cfg, config.DefaultMaxMediaSize, store)
	result := tool.Execute(tools.WithToolContext(context.Background(), "test", "chat"), map[string]any{"prompt": "cat"})
	if result.IsError {
		t.Fatalf("image generation failed: %s", result.ForLLM)
	}
	if !proxyCalled.Load() || len(result.Media) != 1 {
		t.Fatalf("proxy called = %t, media refs = %d", proxyCalled.Load(), len(result.Media))
	}
}

func TestOpenRouterImageUsesBaseRequest(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\n")
	for _, model := range []string{"meta/muse-image", "recraft/recraft-v4"} {
		t.Run(model, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || req.URL.String() != "https://openrouter.ai/api/v1/images" {
					t.Errorf("request = %s %s", req.Method, req.URL)
				}
				if req.Header.Get("Authorization") != "Bearer key" {
					t.Errorf("authorization = %q", req.Header.Get("Authorization"))
				}
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if len(body) != 2 || body["model"] != model || body["prompt"] != "cat" {
					t.Errorf("request body = %#v, want only model and prompt", body)
				}
				response := fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(image))
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(response))),
				}, nil
			})}
			var buf bytes.Buffer
			err := NewImageGenerateTool(nil, config.DefaultMaxMediaSize, nil).generateOpenRouterImage(
				context.Background(), client, "https://openrouter.ai/api/v1", "key", model, "cat", &buf,
			)
			if err != nil || !bytes.Equal(buf.Bytes(), image) {
				t.Fatalf("image = %v, error = %v", buf.Bytes(), err)
			}
		})
	}
}

func TestImageDownloadRejectsNonPublicTargets(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "10.0.0.1", "169.254.169.254", "0.0.0.0", "::1", "fc00::1"} {
		if isPublicImageHost(host) {
			t.Errorf("accepted non-public host %q", host)
		}
		_, err := dialPublicImageHost(context.Background(), "tcp", net.JoinHostPort(host, "443"))
		if err == nil {
			t.Errorf("dialed non-public host %q", host)
		}
	}
	if !isPublicImageHost("8.8.8.8") {
		t.Error("rejected public IP")
	}
}

func TestImageGenerateAllowsConfiguredResponseSize(t *testing.T) {
	response := bytes.Repeat([]byte("x"), 33<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(response)
	}))
	defer server.Close()

	tool := NewImageGenerateTool(nil, 25<<20, nil)
	got, err := tool.postJSON(context.Background(), server.Client(), server.URL, "", "", nil)
	if err != nil {
		t.Fatalf("postJSON rejected response within configured limit: %v", err)
	}
	if len(got) != len(response) {
		t.Fatalf("response length = %d, want %d", len(got), len(response))
	}
}

func TestImageSink_EnforcesLimitAndCapturesHeader(t *testing.T) {
	var buf bytes.Buffer
	sink := newImageSink(&buf, 8)
	if _, err := sink.Write([]byte("12345")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := sink.Write([]byte("67890")); err == nil {
		t.Fatal("expected write past the limit to fail")
	}
	if buf.String() != "12345" {
		t.Fatalf("wrote %q, want only the accepted bytes", buf.String())
	}
	if string(sink.header) != "12345" {
		t.Fatalf("header = %q, want the leading accepted bytes only", sink.header)
	}
}

func TestWriteBase64Image_StreamsToWriter(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\npayload")
	var buf bytes.Buffer
	if err := writeBase64Image(&buf, base64.StdEncoding.EncodeToString(image)); err != nil {
		t.Fatalf("writeBase64Image: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), image) {
		t.Fatalf("decoded = %v, want %v", buf.Bytes(), image)
	}
}

func TestGenerateOpenRouterImage_RejectsOversized(t *testing.T) {
	image := bytes.Repeat([]byte("x"), 4096)
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		response := fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(image))
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(response))),
		}, nil
	})}
	sink := newImageSink(io.Discard, 16) // smaller than the decoded image
	err := NewImageGenerateTool(nil, config.DefaultMaxMediaSize, nil).generateOpenRouterImage(
		context.Background(), client, "https://openrouter.ai/api/v1", "key", "m", "cat", sink,
	)
	if !errors.Is(err, errGeneratedImageTooLarge) {
		t.Fatalf("error = %v, want errGeneratedImageTooLarge", err)
	}
}
