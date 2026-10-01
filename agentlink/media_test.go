package agentlink

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type fakeDownloader struct {
	data  []byte
	err   error
	calls int
}

func (d *fakeDownloader) Download(_ context.Context, _ whatsmeow.DownloadableMessage) ([]byte, error) {
	d.calls++
	return d.data, d.err
}

func TestExtractMediaInlineAndTooLarge(t *testing.T) {
	log := zap.NewNop()

	dl := &fakeDownloader{data: []byte{1, 2, 3}}
	media, caption := ExtractMedia(context.Background(), dl, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("my slip"), Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(3),
	}}, log)
	if len(media) != 1 || caption != "my slip" {
		t.Fatalf("media=%+v caption=%q", media, caption)
	}
	m := media[0]
	if m.Kind != "image" || m.Mime != "image/jpeg" || m.Size != 3 || m.Data != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) || m.TooLarge || m.Caption != "my slip" {
		t.Errorf("image = %+v", m)
	}

	// Exactly 5 MiB still travels inline; one byte more does not, and is not downloaded.
	dl = &fakeDownloader{data: make([]byte, MaxInlineMediaBytes)}
	media, _ = ExtractMedia(context.Background(), dl, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		FileName: proto.String("big.pdf"), Mimetype: proto.String("application/pdf"), FileLength: proto.Uint64(MaxInlineMediaBytes),
	}}, log)
	if media[0].TooLarge || media[0].Data == "" || dl.calls != 1 {
		t.Errorf("5 MiB document = %+v (downloads: %d)", media[0], dl.calls)
	}
	dl = &fakeDownloader{}
	media, _ = ExtractMedia(context.Background(), dl, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		FileName: proto.String("huge.pdf"), Mimetype: proto.String("application/pdf"), FileLength: proto.Uint64(MaxInlineMediaBytes + 1),
	}}, log)
	m = media[0]
	if !m.TooLarge || m.Data != "" || m.Size != MaxInlineMediaBytes+1 || m.Filename != "huge.pdf" || m.Kind != "document" || dl.calls != 0 {
		t.Errorf("too-large document = %+v (downloads: %d)", m, dl.calls)
	}
}

func TestExtractMediaKinds(t *testing.T) {
	log := zap.NewNop()
	for name, c := range map[string]struct {
		msg  *waE2E.Message
		kind string
	}{
		"video":      {&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4")}}, "video"},
		"video note": {&waE2E.Message{PtvMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4")}}, "video"},
		"voice":      {&waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg")}}, "voice"},
		"audio":      {&waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/mpeg")}}, "audio"},
		"sticker":    {&waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp")}}, "sticker"},
	} {
		media, _ := ExtractMedia(context.Background(), &fakeDownloader{data: []byte{9}}, c.msg, log)
		if len(media) != 1 || media[0].Kind != c.kind {
			t.Errorf("%s: %+v", name, media)
		}
	}

	// Things that are not media in v1.
	for name, msg := range map[string]*waE2E.Message{
		"text":     {Conversation: proto.String("hi")},
		"reaction": {ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}},
		"location": {LocationMessage: &waE2E.LocationMessage{}},
		"contact":  {ContactMessage: &waE2E.ContactMessage{}},
		"poll":     {PollCreationMessage: &waE2E.PollCreationMessage{}},
		"nil":      nil,
	} {
		if media, caption := ExtractMedia(context.Background(), &fakeDownloader{}, msg, log); media != nil || caption != "" {
			t.Errorf("%s produced media %+v", name, media)
		}
	}
}

func TestExtractMediaDownloadFailureStillDescribesIt(t *testing.T) {
	media, _ := ExtractMedia(context.Background(), &fakeDownloader{err: errors.New("expired media")}, &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{FileLength: proto.Uint64(100)},
	}, zap.NewNop())
	if len(media) != 1 || media[0].Data != "" || media[0].TooLarge || media[0].Mime != "application/octet-stream" || media[0].Size != 100 {
		t.Errorf("media = %+v", media)
	}
}

func TestContextInfoOf(t *testing.T) {
	text := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("Q1")}}}
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("Q2")}}}
	if ContextInfoOf(text).GetStanzaID() != "Q1" || ContextInfoOf(img).GetStanzaID() != "Q2" {
		t.Error("quote not found")
	}
	if ContextInfoOf(&waE2E.Message{Conversation: proto.String("x")}) != nil || ContextInfoOf(nil) != nil {
		t.Error("expected no context")
	}
}
