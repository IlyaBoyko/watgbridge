// Package tgcustomer is the Telegram customer channel of the Hub: a second bot
// that talks to customers in private chats. Each customer gets a topic in the
// staff group, staff replies in it reach the customer from this bot, and the
// support Agent sees the conversation as `tg:<user_id>`.
//
// The logic lives in Bridge behind small interfaces; glue.go and service.go
// are the only files that touch gotgbot clients or the bridge's global state.
package tgcustomer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"watgbridge/agentlink"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"go.uber.org/zap"
)

// MaxDownloadBytes is the Bot API's limit on what a bot may download.
const MaxDownloadBytes = 20 * 1024 * 1024

// ErrTooBig is what a Downloader returns for a file above MaxDownloadBytes.
var ErrTooBig = errors.New("the file is too big for a bot to download")

const (
	notePrefix = "ℹ️ "
	editPrefix = "✏️ edited:\n"
	// Telegram's limit for a topic name.
	topicNameMax = 128
)

// TopicPost is one post into a customer's topic, made by the Hub bot. Text is
// plain: the implementation does the escaping Telegram needs.
type TopicPost struct {
	// Kind is text, photo, video, animation, voice, audio, document, sticker,
	// location or contact.
	Kind     string
	Prefix   string // the bridge's own words before Text, never bold
	Text     string // the text, or the caption of a file
	Bold     bool   // Text is the customer's own message: show it in bold
	Data     []byte
	Filename string
	Mime     string
	ReplyTo  int64 // a message of the same topic
	Lat, Lon float64
	Phone    string
	First    string
	Last     string
}

// Downloader fetches a file a bot can see. size is what Telegram reported (0
// when unknown). It returns ErrTooBig above MaxDownloadBytes.
type Downloader interface {
	Download(ctx context.Context, fileID string, size int64) ([]byte, error)
}

// Topics is the Hub bot's side: it owns the staff group.
type Topics interface {
	Downloader
	CreateTopic(ctx context.Context, name string) (threadID int64, err error)
	Post(ctx context.Context, threadID int64, p TopicPost) (msgID int64, err error)
}

// Sender is the customer bot's sending side (agentlink.CustomerChannel minus
// the pair bookkeeping).
type Sender interface {
	SendText(ctx context.Context, chatID int64, m agentlink.CustomerText) (int64, error)
	SendFile(ctx context.Context, chatID int64, m agentlink.CustomerFile) (int64, error)
}

// Threads finds and stores the topic of a customer, keyed `tg:<user_id>`.
type Threads interface {
	Find(key string) (threadID int64, found bool, err error)
	Add(key string, threadID int64) error
}

// Events carries what happened to the support Agent (a no-op when the link is
// off).
type Events interface {
	Customer(agentlink.CustomerInput) error
	Staff(agentlink.StaffInput) error
}

// Bridge moves messages between customers and their topics.
type Bridge struct {
	CustomerFiles Downloader // the customer bot's downloads
	Topics        Topics
	Sender        Sender
	Threads       Threads
	Pairs         *Pairs
	Events        Events
	Log           *zap.Logger

	// BoldCustomer shows customers' messages in bold in the topic
	// (telegram.bold_customer_messages).
	BoldCustomer bool

	topicMu sync.Mutex
}

// TopicName is `<first name> <last name> (@username)`, or whatever of it the
// user has.
func TopicName(u *gotgbot.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	switch {
	case name != "" && u.Username != "":
		name += " (@" + u.Username + ")"
	case name == "" && u.Username != "":
		name = "@" + u.Username
	case name == "":
		name = agentlink.TgChatKey(u.Id)
	}
	if r := []rune(name); len(r) > topicNameMax {
		name = string(r[:topicNameMax])
	}
	return name
}

func displayName(u *gotgbot.User) string {
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	return u.Username
}

// topicFor finds the customer's topic or creates it. Updates are handled one
// at a time, but the lock keeps a second caller from ever making a second topic.
func (b *Bridge) topicFor(ctx context.Context, u *gotgbot.User) (int64, error) {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()
	key := agentlink.TgChatKey(u.Id)
	thread, found, err := b.Threads.Find(key)
	if err != nil {
		return 0, fmt.Errorf("topic lookup: %w", err)
	}
	if found {
		return thread, nil
	}
	thread, err = b.Topics.CreateTopic(ctx, TopicName(u))
	if err != nil {
		return 0, fmt.Errorf("could not create the topic: %w", err)
	}
	if err := b.Threads.Add(key, thread); err != nil {
		// The topic exists, so use it; the next message will make another.
		return thread, fmt.Errorf("could not store the topic: %w", err)
	}
	return thread, nil
}

// content is what a customer's message carries, reduced to what the bridge
// needs.
type content struct {
	kind      string // the topic post kind
	mediaKind string // the protocol media kind; "" when there is no file
	label     string // for notes
	fileID    string
	size      int64
	mime      string
	filename  string
	text      string // the text, or the caption
	lat, lon  float64
	phone     string
	first     string
	last      string
}

func largestPhoto(ps []gotgbot.PhotoSize) gotgbot.PhotoSize {
	best := ps[0]
	for _, p := range ps {
		if p.Width*p.Height > best.Width*best.Height || (p.Width*p.Height == best.Width*best.Height && p.FileSize > best.FileSize) {
			best = p
		}
	}
	return best
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// classify returns nil for a message of a kind the bridge does not carry.
func classify(msg *gotgbot.Message) *content {
	switch {
	case msg.Text != "":
		return &content{kind: "text", text: msg.Text}
	case len(msg.Photo) > 0:
		p := largestPhoto(msg.Photo)
		return &content{kind: "photo", mediaKind: "image", label: "photo", fileID: p.FileId, size: p.FileSize,
			mime: "image/jpeg", filename: "photo.jpg", text: msg.Caption}
	case msg.Video != nil:
		v := msg.Video
		return &content{kind: "video", mediaKind: "video", label: "video", fileID: v.FileId, size: v.FileSize,
			mime: orDefault(v.MimeType, "video/mp4"), filename: orDefault(v.FileName, "video.mp4"), text: msg.Caption}
	case msg.VideoNote != nil:
		v := msg.VideoNote
		return &content{kind: "video", mediaKind: "video", label: "video message", fileID: v.FileId, size: v.FileSize,
			mime: "video/mp4", filename: "video_note.mp4"}
	case msg.Animation != nil:
		a := msg.Animation
		return &content{kind: "animation", mediaKind: "video", label: "GIF", fileID: a.FileId, size: a.FileSize,
			mime: orDefault(a.MimeType, "video/mp4"), filename: orDefault(a.FileName, "animation.mp4"), text: msg.Caption}
	case msg.Voice != nil:
		v := msg.Voice
		return &content{kind: "voice", mediaKind: "voice", label: "voice message", fileID: v.FileId, size: v.FileSize,
			mime: orDefault(v.MimeType, "audio/ogg"), filename: "voice.ogg", text: msg.Caption}
	case msg.Audio != nil:
		a := msg.Audio
		return &content{kind: "audio", mediaKind: "audio", label: "audio", fileID: a.FileId, size: a.FileSize,
			mime: orDefault(a.MimeType, "audio/mpeg"), filename: orDefault(a.FileName, "audio.mp3"), text: msg.Caption}
	case msg.Document != nil:
		d := msg.Document
		return &content{kind: "document", mediaKind: "document", label: "file", fileID: d.FileId, size: d.FileSize,
			mime: orDefault(d.MimeType, "application/octet-stream"), filename: orDefault(d.FileName, "file"), text: msg.Caption}
	case msg.Sticker != nil:
		s := msg.Sticker
		mime, name := "image/webp", "sticker.webp"
		switch {
		case s.IsAnimated:
			mime, name = "application/x-tgsticker", "sticker.tgs"
		case s.IsVideo:
			mime, name = "video/webm", "sticker.webm"
		}
		return &content{kind: "sticker", mediaKind: "sticker", label: "sticker", fileID: s.FileId, size: s.FileSize,
			mime: mime, filename: name}
	case msg.Location != nil:
		l := msg.Location
		return &content{kind: "location", lat: l.Latitude, lon: l.Longitude,
			text: "Location: " + strconv.FormatFloat(l.Latitude, 'f', -1, 64) + ", " + strconv.FormatFloat(l.Longitude, 'f', -1, 64)}
	case msg.Contact != nil:
		c := msg.Contact
		name := strings.TrimSpace(c.FirstName + " " + c.LastName)
		return &content{kind: "contact", phone: c.PhoneNumber, first: c.FirstName, last: c.LastName,
			text: strings.TrimSpace("Contact: " + name + " " + c.PhoneNumber)}
	}
	return nil
}

func isMB(n int64) string { return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MB" }

// HandleMessage bridges one update from the customer bot: a new message, or an
// edit of one. Updates from groups, channels and bots are ignored.
func (b *Bridge) HandleMessage(ctx context.Context, msg *gotgbot.Message, edited bool) {
	u := msg.From
	if u == nil || u.IsBot || msg.Chat.Type != "private" || msg.Chat.Id != u.Id {
		return
	}
	thread, err := b.topicFor(ctx, u)
	if err != nil {
		b.Log.Error("customer bot: topic", zap.Int64("user", u.Id), zap.Error(err))
	}
	if edited {
		b.handleEdit(ctx, msg, thread)
		return
	}
	b.handleNew(ctx, msg, thread)
}

// post makes one post into the topic and pairs it with the customer's message.
// It posts nothing when there is no topic: a thread id of 0 would be the
// group's General topic.
func (b *Bridge) post(ctx context.Context, thread int64, p TopicPost, chatID, customerMsgID int64) {
	if thread == 0 {
		return
	}
	id, err := b.Topics.Post(ctx, thread, p)
	if err != nil {
		b.Log.Error("customer bot: could not post into the topic", zap.String("kind", p.Kind), zap.Error(err))
		return
	}
	if customerMsgID == 0 {
		return
	}
	if err := b.Pairs.Record(chatID, customerMsgID, thread, id); err != nil {
		b.Log.Error("customer bot: could not record the message pair", zap.Error(err))
	}
}

func (b *Bridge) quoted(msg *gotgbot.Message) (topicMsg int64, hubID string) {
	r := msg.ReplyToMessage
	if r == nil {
		return 0, ""
	}
	hubID = agentlink.HubMsgIDForTelegram(msg.Chat.Id, r.MessageId)
	if _, id, found, err := b.Pairs.TopicMsgFor(msg.Chat.Id, r.MessageId); err != nil {
		b.Log.Warn("customer bot: pair lookup", zap.Error(err))
	} else if found {
		topicMsg = id
	}
	return topicMsg, hubID
}

func (b *Bridge) handleNew(ctx context.Context, msg *gotgbot.Message, thread int64) {
	u := msg.From
	chatID := msg.Chat.Id
	c := classify(msg)
	if c == nil {
		b.post(ctx, thread, TopicPost{Kind: "text", Text: notePrefix + "The customer sent a message that cannot be bridged (a poll, a dice, a game or a similar kind)."}, chatID, msg.MessageId)
		return
	}
	replyTopic, replyHub := b.quoted(msg)

	var media []agentlink.Media
	if c.fileID == "" {
		b.post(ctx, thread, TopicPost{Kind: c.kind, Text: c.text, Bold: b.BoldCustomer, ReplyTo: replyTopic, Lat: c.lat, Lon: c.lon,
			Phone: c.phone, First: c.first, Last: c.last}, chatID, msg.MessageId)
	} else {
		m := agentlink.Media{Kind: c.mediaKind, Mime: c.mime, Filename: c.filename, Caption: c.text, Size: c.size}
		var data []byte
		failure := ""
		tooBig := c.size > MaxDownloadBytes
		if !tooBig {
			d, err := b.CustomerFiles.Download(ctx, c.fileID, c.size)
			switch {
			case errors.Is(err, ErrTooBig):
				tooBig = true
			case err != nil:
				b.Log.Warn("customer bot: download failed", zap.String("kind", c.kind), zap.Error(err))
				failure = "could not be downloaded, so it was not bridged"
			default:
				data = d
			}
		}
		if tooBig {
			failure = "is over the Bot API's 20 MB download limit, so it was not bridged"
		}
		if data != nil {
			m.Size = int64(len(data))
		}
		if tooBig || m.Size > agentlink.MaxInlineMediaBytes {
			// v1 has no way to fetch it later; the Agent is told it exists.
			m.TooLarge = true
		} else if len(data) > 0 {
			m.Data = base64.StdEncoding.EncodeToString(data)
		}
		media = []agentlink.Media{m}

		if failure == "" {
			b.post(ctx, thread, TopicPost{Kind: c.kind, Text: c.text, Bold: b.BoldCustomer, Data: data, Filename: c.filename, Mime: c.mime, ReplyTo: replyTopic}, chatID, msg.MessageId)
		} else {
			note := notePrefix + "The customer sent a " + c.label
			if c.size > 0 {
				note += " (" + isMB(c.size) + ")"
			}
			note += " that " + failure + "."
			if c.text != "" {
				note += "\n\n" + c.text
			}
			b.post(ctx, thread, TopicPost{Kind: "text", Text: note, ReplyTo: replyTopic}, chatID, msg.MessageId)
		}
	}

	err := b.Events.Customer(agentlink.CustomerInput{
		ChatKey: agentlink.TgChatKey(u.Id), TopicID: thread,
		HubMsgID:    agentlink.HubMsgIDForTelegram(chatID, msg.MessageId),
		ContactName: displayName(u), Username: u.Username,
		Text: c.text, Media: media, ReplyTo: replyHub,
	})
	if err != nil {
		b.Log.Error("customer bot: could not queue the customer message", zap.Error(err))
	}
}

// handleEdit posts "✏️ edited:" replying to the original and reports the edit
// as a new message that says which one it replaces. Only text and captions are
// carried: a live location edits itself every few seconds, and a replaced file
// has no new words.
func (b *Bridge) handleEdit(ctx context.Context, msg *gotgbot.Message, thread int64) {
	u := msg.From
	chatID := msg.Chat.Id
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	if text == "" {
		return
	}
	var replyTopic int64
	if _, id, found, err := b.Pairs.TopicMsgFor(chatID, msg.MessageId); err != nil {
		b.Log.Warn("customer bot: pair lookup", zap.Error(err))
	} else if found {
		replyTopic = id
	}
	b.post(ctx, thread, TopicPost{Kind: "text", Prefix: editPrefix, Text: text, Bold: b.BoldCustomer, ReplyTo: replyTopic}, chatID, msg.MessageId)

	original := agentlink.HubMsgIDForTelegram(chatID, msg.MessageId)
	_, replyHub := b.quoted(msg)
	err := b.Events.Customer(agentlink.CustomerInput{
		ChatKey: agentlink.TgChatKey(u.Id), TopicID: thread,
		// Telegram keeps a message's id when it is edited, but every event needs
		// an id of its own: the edit date tells one edit from the next.
		HubMsgID:    original + ":e" + strconv.FormatInt(msg.EditDate, 10),
		ContactName: displayName(u), Username: u.Username,
		Text: text, ReplyTo: replyHub, EditOf: original,
	})
	if err != nil {
		b.Log.Error("customer bot: could not queue the edit", zap.Error(err))
	}
}
