package tgcustomer

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"watgbridge/agentlink"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestTopicName(t *testing.T) {
	for name, tc := range map[string]struct {
		u    gotgbot.User
		want string
	}{
		"everything":            {gotgbot.User{Id: 1, FirstName: "Wei", LastName: "Tan", Username: "wei_kl"}, "Wei Tan (@wei_kl)"},
		"no last name":          {gotgbot.User{Id: 1, FirstName: "Wei", Username: "wei_kl"}, "Wei (@wei_kl)"},
		"no username":           {gotgbot.User{Id: 1, FirstName: "Wei", LastName: "Tan"}, "Wei Tan"},
		"first name only":       {gotgbot.User{Id: 1, FirstName: "Wei"}, "Wei"},
		"username only":         {gotgbot.User{Id: 1, Username: "wei_kl"}, "@wei_kl"},
		"nothing at all":        {gotgbot.User{Id: 77}, "tg:77"},
		"surrounding spaces":    {gotgbot.User{Id: 1, FirstName: " Wei ", LastName: " "}, "Wei"},
		"too long is cut":       {gotgbot.User{Id: 1, FirstName: strings.Repeat("é", 200)}, strings.Repeat("é", 128)},
		"cut keeps whole runes": {gotgbot.User{Id: 1, FirstName: strings.Repeat("😀", 130)}, strings.Repeat("😀", 128)},
	} {
		if got := TopicName(&tc.u); got != tc.want {
			t.Errorf("%s: TopicName = %q, want %q", name, got, tc.want)
		}
	}
}

// A2: a first message creates a topic with the right name and a tg: thread
// pair; a second message reuses it.
func TestFirstMessageCreatesTopicAndSecondReusesIt(t *testing.T) {
	w := newWorld(t)
	w.handle(textMsg(1, "hello"), false)
	w.handle(textMsg(2, "anyone there?"), false)

	if got := w.topics.Created(); !reflect.DeepEqual(got, []string{"Wei Tan (@wei_kl)"}) {
		t.Fatalf("topics created = %q", got)
	}
	if got := w.threads.adds; !reflect.DeepEqual(got, []string{"tg:5550001111"}) {
		t.Errorf("thread pairs stored under %q", got)
	}
	posts := w.topics.Posts()
	if len(posts) != 2 || posts[0].Thread != 1250 || posts[1].Thread != 1250 || posts[0].Text != "hello" || posts[1].Text != "anyone there?" {
		t.Errorf("posts = %+v", posts)
	}
	for _, ev := range w.events.Customers() {
		if ev.TopicID != 1250 {
			t.Errorf("event topic = %d", ev.TopicID)
		}
	}
}

func TestSimultaneousFirstMessagesMakeOneTopic(t *testing.T) {
	w := newWorld(t)
	var wg sync.WaitGroup
	for i := int64(1); i <= 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.handle(textMsg(i, "hi"), false)
		}()
	}
	wg.Wait()
	if n := len(w.topics.Created()); n != 1 {
		t.Errorf("%d topics created, want 1", n)
	}
}

// A2: group, channel and bot updates, and anything without a person behind it,
// are ignored: no topic, no post, no event.
func TestNonPrivateAndBotUpdatesAreIgnored(t *testing.T) {
	mk := func(chatType string, chatID int64, from *gotgbot.User) *gotgbot.Message {
		m := textMsg(1, "hi")
		m.Chat = gotgbot.Chat{Id: chatID, Type: chatType}
		m.From = from
		return m
	}
	bot := &gotgbot.User{Id: 99, IsBot: true, FirstName: "Other bot"}
	for name, m := range map[string]*gotgbot.Message{
		"group":                  mk("group", -100, customerUser()),
		"supergroup":             mk("supergroup", -1001234, customerUser()),
		"channel":                mk("channel", -1005678, nil),
		"another bot":            mk("private", 99, bot),
		"no sender":              mk("private", customerID, nil),
		"chat is not the sender": mk("private", 12345, customerUser()),
	} {
		w := newWorld(t)
		w.handle(m, false)
		w.handle(m, true)
		if len(w.topics.Created()) != 0 || len(w.topics.Posts()) != 0 || len(w.events.Customers()) != 0 || len(w.files.Calls()) != 0 {
			t.Errorf("%s: something was bridged", name)
		}
	}
}

func TestTopicCreationFailureStillReportsToTheAgentButNeverPostsToGeneral(t *testing.T) {
	w := newWorld(t)
	w.topics.createErr = errors.New("not enough rights")
	w.handle(textMsg(1, "hello"), false)
	if len(w.topics.Posts()) != 0 {
		t.Errorf("posted without a topic: %+v", w.topics.Posts())
	}
	evs := w.events.Customers()
	if len(evs) != 1 || evs[0].TopicID != 0 || evs[0].Text != "hello" {
		t.Errorf("events = %+v", evs)
	}
	// The next message tries again.
	w.topics.createErr = nil
	w.handle(textMsg(2, "hello?"), false)
	if len(w.topics.Created()) != 1 || len(w.topics.Posts()) != 1 {
		t.Errorf("created = %v, posts = %+v", w.topics.Created(), w.topics.Posts())
	}
}

func TestExistingTopicIsReusedAfterARestart(t *testing.T) {
	w := newWorld(t)
	w.threads.m["tg:5550001111"] = 777 // from the live ChatThreadPair table
	w.handle(textMsg(1, "hello"), false)
	if len(w.topics.Created()) != 0 {
		t.Error("a second topic was created for a known customer")
	}
	if p := w.topics.Posts(); len(p) != 1 || p[0].Thread != 777 {
		t.Errorf("posts = %+v", p)
	}
}

// A3: each media type is downloaded with the customer bot and uploaded by the
// Hub bot, caption kept, pair recorded.
func TestMediaIsDownloadedAndReuploaded(t *testing.T) {
	type tcase struct {
		name     string
		build    func(m *gotgbot.Message)
		fileID   string
		wantKind string
		wantMed  string // protocol media kind
		wantMime string
		wantName string
		caption  string
	}
	cases := []tcase{
		{"photo (largest size)", func(m *gotgbot.Message) {
			m.Photo = []gotgbot.PhotoSize{{FileId: "small", Width: 90, Height: 90}, {FileId: "big", Width: 1280, Height: 960, FileSize: 100}, {FileId: "mid", Width: 320, Height: 240}}
			m.Caption = "this one"
		}, "big", "photo", "image", "image/jpeg", "photo.jpg", "this one"},
		{"video", func(m *gotgbot.Message) {
			m.Video = &gotgbot.Video{FileId: "vid", FileName: "clip.mov", MimeType: "video/quicktime", FileSize: 100}
			m.Caption = "see"
		}, "vid", "video", "video", "video/quicktime", "clip.mov", "see"},
		{"voice", func(m *gotgbot.Message) {
			m.Voice = &gotgbot.Voice{FileId: "voice", MimeType: "audio/ogg", FileSize: 100}
		}, "voice", "voice", "voice", "audio/ogg", "voice.ogg", ""},
		{"audio", func(m *gotgbot.Message) {
			m.Audio = &gotgbot.Audio{FileId: "aud", FileName: "song.mp3", MimeType: "audio/mpeg", FileSize: 100}
		}, "aud", "audio", "audio", "audio/mpeg", "song.mp3", ""},
		{"document", func(m *gotgbot.Message) {
			m.Document = &gotgbot.Document{FileId: "doc", FileName: "slip.pdf", MimeType: "application/pdf", FileSize: 100}
			m.Caption = "payment slip"
		}, "doc", "document", "document", "application/pdf", "slip.pdf", "payment slip"},
		{"sticker", func(m *gotgbot.Message) {
			m.Sticker = &gotgbot.Sticker{FileId: "stk", FileSize: 100}
		}, "stk", "sticker", "sticker", "image/webp", "sticker.webp", ""},
		{"animated sticker", func(m *gotgbot.Message) {
			m.Sticker = &gotgbot.Sticker{FileId: "stk", IsAnimated: true, FileSize: 100}
		}, "stk", "sticker", "sticker", "application/x-tgsticker", "sticker.tgs", ""},
		{"video sticker", func(m *gotgbot.Message) {
			m.Sticker = &gotgbot.Sticker{FileId: "stk", IsVideo: true, FileSize: 100}
		}, "stk", "sticker", "sticker", "video/webm", "sticker.webm", ""},
		{"gif", func(m *gotgbot.Message) {
			m.Animation = &gotgbot.Animation{FileId: "gif", FileSize: 100}
			m.Caption = "lol"
		}, "gif", "animation", "video", "video/mp4", "animation.mp4", "lol"},
		{"video message", func(m *gotgbot.Message) {
			m.VideoNote = &gotgbot.VideoNote{FileId: "vn", FileSize: 100}
		}, "vn", "video", "video", "video/mp4", "video_note.mp4", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			content := []byte("bytes of " + tc.fileID)
			w.files.files[tc.fileID] = content
			m := privateMsg(42)
			tc.build(m)
			w.handle(m, false)

			if got := w.files.Calls(); !reflect.DeepEqual(got, []string{tc.fileID}) {
				t.Fatalf("customer bot downloads = %v", got)
			}
			posts := w.topics.Posts()
			if len(posts) != 1 {
				t.Fatalf("posts = %+v", posts)
			}
			p := posts[0]
			if p.Kind != tc.wantKind || string(p.Data) != string(content) || p.Text != tc.caption || p.Filename != tc.wantName || p.Mime != tc.wantMime {
				t.Errorf("post = %+v", p.TopicPost)
			}
			if _, topicMsg, found, _ := w.pairs.TopicMsgFor(customerID, 42); !found || topicMsg != p.ID {
				t.Errorf("pair: topic msg %d found=%v, want %d", topicMsg, found, p.ID)
			}

			evs := w.events.Customers()
			if len(evs) != 1 || len(evs[0].Media) != 1 {
				t.Fatalf("events = %+v", evs)
			}
			med := evs[0].Media[0]
			if med.Kind != tc.wantMed || med.Mime != tc.wantMime || med.Filename != tc.wantName || med.Caption != tc.caption ||
				med.Data != base64.StdEncoding.EncodeToString(content) || med.Size != int64(len(content)) || med.TooLarge {
				t.Errorf("media = %+v", med)
			}
			if evs[0].Text != tc.caption {
				t.Errorf("event text = %q, want the caption %q", evs[0].Text, tc.caption)
			}
		})
	}
}

func TestLocationContactAndUnsupportedMessages(t *testing.T) {
	w := newWorld(t)

	m := privateMsg(1)
	m.Location = &gotgbot.Location{Latitude: 3.139003, Longitude: 101.686855}
	w.handle(m, false)

	m = privateMsg(2)
	m.Contact = &gotgbot.Contact{PhoneNumber: "+60123456789", FirstName: "Aiman", LastName: "Yusof"}
	w.handle(m, false)

	m = privateMsg(3)
	m.Poll = &gotgbot.Poll{Id: "p"}
	w.handle(m, false)

	posts := w.topics.Posts()
	if len(posts) != 3 {
		t.Fatalf("posts = %+v", posts)
	}
	if posts[0].Kind != "location" || posts[0].Lat != 3.139003 || posts[0].Lon != 101.686855 {
		t.Errorf("location post = %+v", posts[0].TopicPost)
	}
	if posts[1].Kind != "contact" || posts[1].Phone != "+60123456789" || posts[1].First != "Aiman" || posts[1].Last != "Yusof" {
		t.Errorf("contact post = %+v", posts[1].TopicPost)
	}
	if posts[2].Kind != "text" || !strings.Contains(posts[2].Text, "cannot be bridged") {
		t.Errorf("unsupported post = %+v", posts[2].TopicPost)
	}

	evs := w.events.Customers()
	if len(evs) != 2 {
		t.Fatalf("%d events, want 2 (the unsupported message is not one)", len(evs))
	}
	if evs[0].Text != "Location: 3.139003, 101.686855" || evs[1].Text != "Contact: Aiman Yusof +60123456789" {
		t.Errorf("event texts = %q, %q", evs[0].Text, evs[1].Text)
	}
}

// A3: above the Bot API's limit a note replaces the file, and the Agent is told
// the media exists (too_large) without any data.
func TestOversizedMediaGivesANote(t *testing.T) {
	for name, tc := range map[string]struct {
		size    int64
		dlErr   error
		wantDls int
	}{
		"size reported over 20 MB":   {21 * 1024 * 1024, nil, 0},
		"Telegram refuses (no size)": {0, ErrTooBig, 1},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.files.errs["big"] = tc.dlErr
			m := privateMsg(42)
			m.Video = &gotgbot.Video{FileId: "big", FileName: "film.mp4", MimeType: "video/mp4", FileSize: tc.size}
			m.Caption = "the whole thing"
			w.handle(m, false)

			if n := len(w.files.Calls()); n != tc.wantDls {
				t.Errorf("%d downloads, want %d", n, tc.wantDls)
			}
			posts := w.topics.Posts()
			if len(posts) != 1 || posts[0].Kind != "text" || !strings.Contains(posts[0].Text, "20 MB") ||
				!strings.Contains(posts[0].Text, "video") || !strings.HasSuffix(posts[0].Text, "the whole thing") || posts[0].Data != nil {
				t.Fatalf("posts = %+v", posts)
			}
			if _, _, found, _ := w.pairs.TopicMsgFor(customerID, 42); !found {
				t.Error("the note is not paired with the customer's message")
			}
			evs := w.events.Customers()
			if len(evs) != 1 || len(evs[0].Media) != 1 {
				t.Fatalf("events = %+v", evs)
			}
			med := evs[0].Media[0]
			if !med.TooLarge || med.Data != "" || med.Kind != "video" || med.Filename != "film.mp4" || med.Caption != "the whole thing" {
				t.Errorf("media = %+v", med)
			}
			if evs[0].Text != "the whole thing" {
				t.Errorf("event text = %q", evs[0].Text)
			}
		})
	}
}

func TestFailedDownloadGivesANoteAndMetadataOnly(t *testing.T) {
	w := newWorld(t)
	w.files.errs["doc"] = errors.New("connection reset")
	m := privateMsg(42)
	m.Document = &gotgbot.Document{FileId: "doc", FileName: "slip.pdf", MimeType: "application/pdf", FileSize: 1000}
	w.handle(m, false)

	posts := w.topics.Posts()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "could not be downloaded") {
		t.Fatalf("posts = %+v", posts)
	}
	med := w.events.Customers()[0].Media[0]
	if med.Data != "" || med.TooLarge || med.Size != 1000 || med.Kind != "document" {
		t.Errorf("media = %+v", med)
	}
}

// A4: media travels inline up to 5 MiB and as too_large above it, but the
// topic still gets the file (the Bot API allows 20 MB).
func TestInlineMediaLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		n        int
		tooLarge bool
	}{
		"exactly 5 MiB":    {agentlink.MaxInlineMediaBytes, false},
		"5 MiB and a byte": {agentlink.MaxInlineMediaBytes + 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.files.files["doc"] = make([]byte, tc.n)
			m := privateMsg(42)
			m.Document = &gotgbot.Document{FileId: "doc", FileName: "x.bin", FileSize: int64(tc.n)}
			w.handle(m, false)

			posts := w.topics.Posts()
			if len(posts) != 1 || len(posts[0].Data) != tc.n {
				t.Fatalf("the topic must get the whole file; posts = %d", len(posts))
			}
			med := w.events.Customers()[0].Media[0]
			if med.TooLarge != tc.tooLarge || (med.Data == "") != tc.tooLarge || med.Size != int64(tc.n) {
				t.Errorf("too_large=%v data=%d size=%d", med.TooLarge, len(med.Data), med.Size)
			}
		})
	}
}

// A4: the event carries the conversation, the <chat>:<message> id, contact,
// text and the quote as a hub_msg_id; the topic post quotes the mirrored message.
func TestCustomerMessageEventAndQuote(t *testing.T) {
	w := newWorld(t)
	w.handle(textMsg(40, "do you ship to Penang?"), false)
	firstTopicMsg := w.topics.Posts()[0].ID

	reply := textMsg(42, "Is BPC-157 in stock?")
	reply.ReplyToMessage = textMsg(40, "do you ship to Penang?")
	w.handle(reply, false)

	posts := w.topics.Posts()
	if len(posts) != 2 || posts[1].ReplyTo != firstTopicMsg {
		t.Errorf("the topic post must quote the mirror of message 40 (%d); posts = %+v", firstTopicMsg, posts)
	}
	evs := w.events.Customers()
	want := agentlink.CustomerInput{
		ChatKey: "tg:5550001111", TopicID: 1250, HubMsgID: "5550001111:42", ContactName: "Wei Tan", Username: "wei_kl",
		Text: "Is BPC-157 in stock?", ReplyTo: "5550001111:40",
	}
	if len(evs) != 2 || !reflect.DeepEqual(evs[1], want) {
		t.Errorf("event = %+v\nwant   %+v", evs[1], want)
	}
	if evs[0].HubMsgID != "5550001111:40" || evs[0].ReplyTo != "" {
		t.Errorf("first event = %+v", evs[0])
	}

	// A quote of a message the Hub never paired still names it for the Agent.
	odd := textMsg(43, "and this?")
	odd.ReplyToMessage = textMsg(11, "old")
	w.handle(odd, false)
	if last := w.events.Customers()[2]; last.ReplyTo != "5550001111:11" {
		t.Errorf("reply_to = %q", last.ReplyTo)
	}
	if p := w.topics.Posts()[2]; p.ReplyTo != 0 {
		t.Errorf("the topic post quoted %d, an unpaired message", p.ReplyTo)
	}
}

func TestCustomerQuotingTheBotsOwnMessageQuotesTheMirror(t *testing.T) {
	w := newWorld(t)
	// The Agent's reply 7001 was mirrored as topic message 9500.
	if err := w.pairs.Record(customerID, 7001, 1250, 9500); err != nil {
		t.Fatal(err)
	}
	w.threads.m["tg:5550001111"] = 1250
	m := textMsg(50, "thanks, and the price?")
	m.ReplyToMessage = textMsg(7001, "In stock")
	w.handle(m, false)
	if p := w.topics.Posts()[0]; p.ReplyTo != 9500 {
		t.Errorf("ReplyTo = %d, want 9500", p.ReplyTo)
	}
	if ev := w.events.Customers()[0]; ev.ReplyTo != "5550001111:7001" {
		t.Errorf("reply_to = %q", ev.ReplyTo)
	}
}

// A3 + A4: an edit posts "✏️ edited:" replying to the original and is reported
// as a new message with edit_of.
func TestEditedMessage(t *testing.T) {
	w := newWorld(t)
	w.handle(textMsg(41, "is it in stok?"), false)
	original := w.topics.Posts()[0].ID

	edit := textMsg(41, "is it in stock?")
	edit.EditDate = 1790000000
	w.handle(edit, true)

	posts := w.topics.Posts()
	if len(posts) != 2 {
		t.Fatalf("posts = %+v", posts)
	}
	if posts[1].Text != "✏️ edited:\nis it in stock?" || posts[1].ReplyTo != original || posts[1].Kind != "text" {
		t.Errorf("edit post = %+v", posts[1])
	}
	// A reply to the edit post still reaches the customer's message 41.
	if chat, msg, found, _ := w.pairs.CustomerMsgFor(1250, posts[1].ID); !found || chat != customerID || msg != 41 {
		t.Errorf("edit post pair = %d %d %v", chat, msg, found)
	}

	evs := w.events.Customers()
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	e := evs[1]
	if e.EditOf != "5550001111:41" || e.Text != "is it in stock?" || e.HubMsgID != "5550001111:41:e1790000000" || e.HubMsgID == evs[0].HubMsgID {
		t.Errorf("edit event = %+v", e)
	}

	// A second edit is a different event.
	edit2 := textMsg(41, "is it in stock now?")
	edit2.EditDate = 1790000100
	w.handle(edit2, true)
	if e2 := w.events.Customers()[2]; e2.HubMsgID == e.HubMsgID || e2.EditOf != "5550001111:41" {
		t.Errorf("second edit event = %+v", e2)
	}
}

func TestEditOfAFileCaptionAndOfUnpairedMessages(t *testing.T) {
	w := newWorld(t)
	w.threads.m["tg:5550001111"] = 1250

	// A caption edit counts; the file does not travel again.
	capMsg := privateMsg(60)
	capMsg.Photo = []gotgbot.PhotoSize{{FileId: "p", Width: 1, Height: 1}}
	capMsg.Caption = "new caption"
	capMsg.EditDate = 5
	w.handle(capMsg, true)
	if p := w.topics.Posts(); len(p) != 1 || p[0].Text != "✏️ edited:\nnew caption" || p[0].ReplyTo != 0 || p[0].Data != nil {
		t.Errorf("posts = %+v", p)
	}
	if len(w.files.Calls()) != 0 {
		t.Error("an edit downloaded a file")
	}

	// Nothing to say (a live location moving, a swapped file without words): ignored.
	live := privateMsg(61)
	live.Location = &gotgbot.Location{Latitude: 1, Longitude: 2}
	live.EditDate = 6
	w.handle(live, true)
	if len(w.topics.Posts()) != 1 || len(w.events.Customers()) != 1 {
		t.Error("a location edit was bridged")
	}
}

func TestEventFailureDoesNotStopTheTopicPost(t *testing.T) {
	w := newWorld(t)
	w.events.err = fmt.Errorf("outbox full")
	w.handle(textMsg(1, "hello"), false)
	if len(w.topics.Posts()) != 1 {
		t.Error("the topic post depends on the event")
	}
}

func TestPostFailureStillReportsTheMessage(t *testing.T) {
	w := newWorld(t)
	w.threads.m["tg:5550001111"] = 1250
	w.topics.postErr = errors.New("telegram down")
	w.handle(textMsg(1, "hello"), false)
	if len(w.events.Customers()) != 1 {
		t.Error("the Agent did not hear about a message the topic could not show")
	}
	if _, _, found, _ := w.pairs.TopicMsgFor(customerID, 1); found {
		t.Error("a pair was recorded for a post that failed")
	}
}
