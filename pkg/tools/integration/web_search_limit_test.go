package integrationtools

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// countingBody is an io.ReadCloser that yields a large, bounded number of
// bytes and records how many were actually read, so tests can assert that a
// provider clamps its response buffering.
type countingBody struct {
	remaining int
	read      int
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.remaining {
		n = b.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	b.remaining -= n
	b.read += n
	return n, nil
}

func (b *countingBody) Close() error { return nil }

// oversizedClient points a provider at an in-memory response without a real
// server, reusing the roundTripFunc defined in web_test.go.
func oversizedClient(body *countingBody) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
				Request:    &http.Request{},
			}, nil
		}),
	}
}

// TestSearchProviders_ClampResponseSize verifies that every search provider
// bounds how much of an upstream response it buffers in memory.
func TestSearchProviders_ClampResponseSize(t *testing.T) {
	// Deliberately larger than maxSearchResponseBytes so the clamp is exercised.
	const payload = maxSearchResponseBytes + (1 << 20)

	tests := []struct {
		name     string
		provider func(*http.Client) searchProvider
	}{
		{
			name: "brave",
			provider: func(c *http.Client) searchProvider {
				return &BraveSearchProvider{keyPool: NewAPIKeyPool([]string{"test-key"}), client: c}
			},
		},
		{
			name: "tavily",
			provider: func(c *http.Client) searchProvider {
				return &TavilySearchProvider{keyPool: NewAPIKeyPool([]string{"test-key"}), client: c}
			},
		},
		{
			name: "duckduckgo",
			provider: func(c *http.Client) searchProvider {
				return &DuckDuckGoSearchProvider{client: c}
			},
		},
		{
			name: "perplexity",
			provider: func(c *http.Client) searchProvider {
				return &PerplexitySearchProvider{keyPool: NewAPIKeyPool([]string{"test-key"}), client: c}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &countingBody{remaining: payload}
			p := tt.provider(oversizedClient(body))

			if _, err := p.Search(context.Background(), "picoclaw", 5, ""); err != nil {
				// The oversized body is not valid JSON/HTML, so an error is
				// expected. We only care that the read was clamped.
				t.Logf("Search returned (expected) error: %v", err)
			}

			if body.read > maxSearchResponseBytes {
				t.Fatalf("provider read %d bytes from the response, want <= %d",
					body.read, maxSearchResponseBytes)
			}
		})
	}
}

// searchProvider is the minimal interface shared by the search providers under
// test, letting the table drive them uniformly.
type searchProvider interface {
	Search(ctx context.Context, query string, count int, rangeCode string) (string, error)
}
