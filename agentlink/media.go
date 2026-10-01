package agentlink

import (
	"context"
	"encoding/base64"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.uber.org/zap"
)

// Downloader fetches and decrypts a WhatsApp media message. *whatsmeow.Client
// satisfies it.
type Downloader interface {
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

// ExtractMedia describes the media a WhatsApp message carries, downloading it
// when it is small enough to travel inline. It returns the caption too, which
// becomes the event's text. Messages with no media return nil.
//
// Reactions, polls, locations and contacts are not media and produce nothing
// in protocol v1.
func ExtractMedia(ctx context.Context, dl Downloader, msg *waE2E.Message, log *zap.Logger) (media []Media, caption string) {
	var (
		m    Media
		body whatsmeow.DownloadableMessage
	)
	switch {
	case msg.GetImageMessage() != nil:
		im := msg.GetImageMessage()
		m = Media{Kind: "image", Mime: im.GetMimetype(), Size: int64(im.GetFileLength()), Caption: im.GetCaption()}
		body = im
	case msg.GetVideoMessage() != nil:
		vm := msg.GetVideoMessage()
		m = Media{Kind: "video", Mime: vm.GetMimetype(), Size: int64(vm.GetFileLength()), Caption: vm.GetCaption()}
		body = vm
	case msg.GetPtvMessage() != nil:
		vm := msg.GetPtvMessage()
		m = Media{Kind: "video", Mime: vm.GetMimetype(), Size: int64(vm.GetFileLength()), Caption: vm.GetCaption()}
		body = vm
	case msg.GetAudioMessage() != nil:
		am := msg.GetAudioMessage()
		kind := "audio"
		if am.GetPTT() {
			kind = "voice"
		}
		m = Media{Kind: kind, Mime: am.GetMimetype(), Size: int64(am.GetFileLength())}
		body = am
	case msg.GetDocumentMessage() != nil:
		dm := msg.GetDocumentMessage()
		m = Media{Kind: "document", Mime: dm.GetMimetype(), Size: int64(dm.GetFileLength()),
			Filename: dm.GetFileName(), Caption: dm.GetCaption()}
		body = dm
	case msg.GetStickerMessage() != nil:
		sm := msg.GetStickerMessage()
		m = Media{Kind: "sticker", Mime: sm.GetMimetype(), Size: int64(sm.GetFileLength())}
		body = sm
	default:
		return nil, ""
	}
	if m.Mime == "" {
		m.Mime = "application/octet-stream"
	}

	if m.Size > MaxInlineMediaBytes {
		// v1 has no way to fetch it later; the Agent is told it exists.
		m.TooLarge = true
	} else if data, err := dl.Download(ctx, body); err != nil {
		log.Warn("agent link: could not download media for the agent", zap.String("kind", m.Kind), zap.Error(err))
	} else {
		m.Size = int64(len(data))
		if len(data) > 0 {
			m.Data = base64.StdEncoding.EncodeToString(data)
		}
	}
	return []Media{m}, m.Caption
}

// ContextInfoOf returns the quote/forward context of the message kinds the
// link reports. It is the subset of the bridge's own extractor that matters
// here (a customer replying to a message).
func ContextInfoOf(msg *waE2E.Message) *waE2E.ContextInfo {
	for _, ci := range []*waE2E.ContextInfo{
		msg.GetExtendedTextMessage().GetContextInfo(),
		msg.GetImageMessage().GetContextInfo(),
		msg.GetVideoMessage().GetContextInfo(),
		msg.GetPtvMessage().GetContextInfo(),
		msg.GetAudioMessage().GetContextInfo(),
		msg.GetDocumentMessage().GetContextInfo(),
		msg.GetStickerMessage().GetContextInfo(),
	} {
		if ci != nil {
			return ci
		}
	}
	return nil
}
