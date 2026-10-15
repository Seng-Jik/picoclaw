package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/config"
)

func stubSuccessCaller(t *testing.T) *stubCaller {
	t.Helper()
	return &stubCaller{callFn: func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
		return successResponse(t), nil
	}}
}

func countAPICalls(caller *stubCaller, needle string) int {
	count := 0
	for _, call := range caller.calls {
		if strings.Contains(call.URL, needle) {
			count++
		}
	}
	return count
}

func TestIsTransientTelegramDownloadError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "real dns i/o timeout",
			err: errors.New(
				"telego: getFile: internal execution: request call: fasthttp do request: lookup api.telegram.org: i/o timeout",
			),
			want: true,
		},
		{name: "connection reset", err: errors.New("read tcp 1.2.3.4: connection reset by peer"), want: true},
		{name: "empty download", err: errTelegramDownloadFailed, want: true},
		{name: "nil", err: nil, want: false},
		{name: "context canceled", err: context.Canceled, want: false},
		{name: "permanent bad request", err: errors.New(`api: 400 "Bad Request: file not found"`), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTransientTelegramDownloadError(tc.err))
		})
	}
}

func TestDownloadWithRetry_RetriesTransientErrors(t *testing.T) {
	ch := newTestChannel(t, stubSuccessCaller(t))

	attempts := 0
	got := ch.downloadWithRetry(context.Background(), "file-1", func(context.Context) (string, error) {
		attempts++
		if attempts < telegramDownloadMaxAttempts {
			return "", errors.New("telego: getFile: fasthttp do request: lookup api.telegram.org: i/o timeout")
		}
		return "/tmp/voice.ogg", nil
	})

	require.Equal(t, "/tmp/voice.ogg", got)
	require.Equal(t, telegramDownloadMaxAttempts, attempts)
}

func TestDownloadWithRetry_DoesNotRetryPermanentError(t *testing.T) {
	ch := newTestChannel(t, stubSuccessCaller(t))

	attempts := 0
	got := ch.downloadWithRetry(context.Background(), "file-1", func(context.Context) (string, error) {
		attempts++
		return "", errors.New(`api: 400 "Bad Request: file not found"`)
	})

	require.Empty(t, got)
	require.Equal(t, 1, attempts)
}

func TestDownloadWithRetry_StopsOnContextCancellation(t *testing.T) {
	ch := newTestChannel(t, stubSuccessCaller(t))

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	got := ch.downloadWithRetry(ctx, "file-1", func(context.Context) (string, error) {
		attempts++
		cancel()
		return "", errors.New("lookup api.telegram.org: i/o timeout")
	})

	require.Empty(t, got)
	require.Equal(t, 1, attempts)
}

// TestDownloadFile_RetriesTransientGetFileFailure exercises the full downloadFile
// path against a local Telegram-compatible server: the first getFile call fails
// with a transient error and the retry must succeed.
func TestDownloadFile_RetriesTransientGetFileFailure(t *testing.T) {
	var getFileCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			getFileCalls++
			w.Header().Set("Content-Type", "application/json")
			if getFileCalls == 1 {
				_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"timeout contacting file server"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"file_id":        "voice-file",
					"file_unique_id": "uniq",
					"file_path":      "voice/file_1.oga",
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("audio-bytes"))
	}))
	defer server.Close()

	ch, err := NewTelegramChannel(
		&config.Channel{Type: config.ChannelTelegram, Enabled: true},
		&config.TelegramSettings{
			Token:   *config.NewSecureString(testToken),
			BaseURL: server.URL,
		},
		nil,
	)
	require.NoError(t, err)

	path := ch.downloadFile(context.Background(), "voice-file", ".ogg")
	require.NotEmpty(t, path, "download should recover after a transient getFile failure")
	t.Cleanup(func() { _ = os.Remove(path) })
	require.GreaterOrEqual(t, getFileCalls, 2)
}

// TestHandleMessage_NotifiesUserWhenVoiceDownloadFails pins the silent-drop bug:
// when a voice message cannot be downloaded, the user must receive a visible
// notice instead of the message being ignored.
func TestHandleMessage_NotifiesUserWhenVoiceDownloadFails(t *testing.T) {
	caller := &stubCaller{
		callFn: func(_ context.Context, url string, _ *ta.RequestData) (*ta.Response, error) {
			switch {
			case strings.Contains(url, "getFile"):
				return nil, errors.New("telego: getFile: lookup api.telegram.org: i/o timeout")
			case strings.Contains(url, "sendMessage"):
				return successResponse(t), nil
			default:
				t.Fatalf("unexpected API call: %s", url)
				return nil, nil
			}
		},
	}
	ch := newTestChannel(t, caller)
	ch.ctx = context.Background()

	msg := &telego.Message{
		MessageID: 7,
		Chat:      telego.Chat{ID: 42, Type: "private"},
		From:      &telego.User{ID: 99, FirstName: "Alice"},
		Voice:     &telego.Voice{FileID: "voice-file"},
	}

	require.NoError(t, ch.handleMessage(context.Background(), msg))

	require.Equal(t, telegramDownloadMaxAttempts, countAPICalls(caller, "getFile"))
	require.Equal(t, 1, countAPICalls(caller, "sendMessage"), "user must be told the audio was dropped")
}
