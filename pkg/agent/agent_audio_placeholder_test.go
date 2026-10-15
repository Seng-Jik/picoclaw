package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// placeholderRecordingManager records SendPlaceholder calls so tests can assert
// that audio messages always produce visible feedback.
type placeholderRecordingManager struct {
	recordingChannelManager
	placeholders []string
}

func (m *placeholderRecordingManager) SendPlaceholder(_ context.Context, channel, chatID string) bool {
	m.placeholders = append(m.placeholders, channel+":"+chatID)
	return true
}

func TestPrepareInboundMessageForAgent_SendsPlaceholderForAudioWithoutTranscriber(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t) //nolint:dogsled
	defer cleanup()

	cm := &placeholderRecordingManager{}
	al.channelManager = cm
	// No transcriber configured: transcription is skipped, but the user must
	// still see the placeholder that the channel deferred for audio messages.

	msg := testInboundMessage(bus.InboundMessage{
		Channel:  "telegram",
		ChatID:   "chat-1",
		SenderID: "user-1",
		Content:  "[voice]",
	})

	al.prepareInboundMessageForAgent(context.Background(), msg)

	if len(cm.placeholders) != 1 {
		t.Fatalf("placeholder calls = %v, want exactly one", cm.placeholders)
	}
	if cm.placeholders[0] != "telegram:chat-1" {
		t.Fatalf("placeholder target = %q, want %q", cm.placeholders[0], "telegram:chat-1")
	}
}

func TestPrepareInboundMessageForAgent_NoPlaceholderForText(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t) //nolint:dogsled
	defer cleanup()

	cm := &placeholderRecordingManager{}
	al.channelManager = cm

	msg := testInboundMessage(bus.InboundMessage{
		Channel:  "telegram",
		ChatID:   "chat-1",
		SenderID: "user-1",
		Content:  "just a plain text message",
	})

	al.prepareInboundMessageForAgent(context.Background(), msg)

	if len(cm.placeholders) != 0 {
		t.Fatalf("placeholder calls = %v, want none for a text message", cm.placeholders)
	}
}
