package tgcustomer

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"watgbridge/agentlink"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// staffMsg is a message a staff member writes in the customer's topic.
func staffMsg(id int64) *gotgbot.Message {
	return &gotgbot.Message{
		MessageId: id, MessageThreadId: 1250, Date: 1, IsTopicMessage: true,
		From: &gotgbot.User{Id: 704338780, FirstName: "Ilya", LastName: "Boyko"},
		Chat: gotgbot.Chat{Id: -1001234567890, Type: "supergroup"},
	}
}

// A5: a staff reply goes out through the customer bot as HTML (so formatting
// and special characters survive), the pair is recorded, and staff.message is
// reported with the message as the customer received it.
func TestStaffTextReachesTheCustomer(t *testing.T) {
	w := newWorld(t)
	m := staffMsg(500)
	m.Text = "Yes, 2 < 3 days & in stock"
	m.Entities = []gotgbot.MessageEntity{{Type: "bold", Offset: 0, Length: 3}}

	if err := w.bridge.StaffToCustomer(t.Context(), m, customerID); err != nil {
		t.Fatal(err)
	}
	sent := w.sender.Sent()
	if len(sent) != 1 || sent[0].File || sent[0].ChatID != customerID || sent[0].Text.HTML != "<b>Yes</b>, 2 &lt; 3 days &amp; in stock" || sent[0].Text.ReplyTo != 0 {
		t.Fatalf("sent = %+v", sent)
	}
	if chat, msg, found, _ := w.pairs.CustomerMsgFor(1250, 500); !found || chat != customerID || msg != 7001 {
		t.Errorf("pair = %d %d %v", chat, msg, found)
	}
	evs := w.events.Staffs()
	want := agentlink.StaffInput{
		ChatKey: "tg:5550001111", TopicID: 1250, HubMsgID: "5550001111:7001", Source: "topic",
		TgUserID: 704338780, Name: "Ilya Boyko", Text: "Yes, 2 < 3 days & in stock",
	}
	if len(evs) != 1 || !reflect.DeepEqual(evs[0], want) {
		t.Errorf("staff event = %+v\nwant         %+v", evs, want)
	}
}

// A5: quoting a bridged customer message quotes the customer's original.
func TestStaffQuoteMapsToTheCustomersMessage(t *testing.T) {
	w := newWorld(t)
	if err := w.pairs.Record(customerID, 42, 1250, 9100); err != nil {
		t.Fatal(err)
	}
	m := staffMsg(501)
	m.Text = "Yes"
	m.ReplyToMessage = &gotgbot.Message{MessageId: 9100, MessageThreadId: 1250}
	if err := w.bridge.StaffToCustomer(t.Context(), m, customerID); err != nil {
		t.Fatal(err)
	}
	if got := w.sender.Sent()[0].Text.ReplyTo; got != 42 {
		t.Errorf("ReplyTo = %d, want 42", got)
	}
	if ev := w.events.Staffs()[0]; ev.ReplyTo != "5550001111:42" {
		t.Errorf("reply_to = %q", ev.ReplyTo)
	}

	// The "reply" every topic message carries to the topic's creation is no quote.
	m2 := staffMsg(502)
	m2.Text = "Hello again"
	m2.ReplyToMessage = &gotgbot.Message{MessageId: 1250, ForumTopicCreated: &gotgbot.ForumTopicCreated{Name: "Wei"}}
	// A quote of something that is not bridged is no quote either.
	m3 := staffMsg(503)
	m3.Text = "And?"
	m3.ReplyToMessage = &gotgbot.Message{MessageId: 12345}
	for _, m := range []*gotgbot.Message{m2, m3} {
		if err := w.bridge.StaffToCustomer(t.Context(), m, customerID); err != nil {
			t.Fatal(err)
		}
	}
	sent := w.sender.Sent()
	if sent[1].Text.ReplyTo != 0 || sent[2].Text.ReplyTo != 0 {
		t.Errorf("unexpected quotes: %d, %d", sent[1].Text.ReplyTo, sent[2].Text.ReplyTo)
	}
}

// Staff quoting the mirror of an Agent reply (or one of their own replies)
// quotes that message in the customer's chat.
func TestStaffQuoteOfAMirrorQuotesTheCustomerChatMessage(t *testing.T) {
	w := newWorld(t)
	if err := w.pairs.Record(customerID, 7001, 1250, 9500); err != nil { // the Agent's reply and its mirror
		t.Fatal(err)
	}
	m := staffMsg(504)
	m.Text = "Correction"
	m.ReplyToMessage = &gotgbot.Message{MessageId: 9500}
	if err := w.bridge.StaffToCustomer(t.Context(), m, customerID); err != nil {
		t.Fatal(err)
	}
	if got := w.sender.Sent()[0].Text.ReplyTo; got != 7001 {
		t.Errorf("ReplyTo = %d, want 7001", got)
	}
}

// A5: files belong to the Hub bot, so they are downloaded from it and uploaded
// by the customer bot. A photo-only reply is still reported.
func TestStaffFilesAreDownloadedAndReuploaded(t *testing.T) {
	type tcase struct {
		name    string
		build   func(m *gotgbot.Message)
		fileID  string
		kind    string
		mime    string
		file    string
		caption string
		plain   string
	}
	for _, tc := range []tcase{
		{"photo only", func(m *gotgbot.Message) {
			m.Photo = []gotgbot.PhotoSize{{FileId: "s", Width: 10, Height: 10}, {FileId: "l", Width: 800, Height: 600}}
		}, "l", "image", "image/jpeg", "photo.jpg", "", ""},
		{"photo with caption", func(m *gotgbot.Message) {
			m.Photo = []gotgbot.PhotoSize{{FileId: "l", Width: 800, Height: 600}}
			m.Caption = "the <label>"
		}, "l", "image", "image/jpeg", "photo.jpg", "the &lt;label&gt;", "the <label>"},
		{"video", func(m *gotgbot.Message) {
			m.Video = &gotgbot.Video{FileId: "v", FileName: "clip.mp4", MimeType: "video/mp4"}
		}, "v", "video", "video/mp4", "clip.mp4", "", ""},
		{"document", func(m *gotgbot.Message) {
			m.Document = &gotgbot.Document{FileId: "d", FileName: "invoice.pdf", MimeType: "application/pdf"}
			m.Caption = "invoice"
		}, "d", "document", "application/pdf", "invoice.pdf", "invoice", "invoice"},
		{"voice", func(m *gotgbot.Message) {
			m.Voice = &gotgbot.Voice{FileId: "vo", MimeType: "audio/ogg"}
		}, "vo", "voice", "audio/ogg", "voice.ogg", "", ""},
		{"sticker", func(m *gotgbot.Message) {
			m.Sticker = &gotgbot.Sticker{FileId: "st", IsAnimated: true}
		}, "st", "sticker", "", "sticker.tgs", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			content := []byte("content of " + tc.fileID)
			w.topics.files[tc.fileID] = content
			m := staffMsg(600)
			tc.build(m)
			if err := w.bridge.StaffToCustomer(t.Context(), m, customerID); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(w.topics.downloads, []string{tc.fileID}) {
				t.Errorf("downloads from the Hub bot = %v", w.topics.downloads)
			}
			sent := w.sender.Sent()
			if len(sent) != 1 || !sent[0].File {
				t.Fatalf("sent = %+v", sent)
			}
			f := sent[0].Media
			if f.Kind != tc.kind || string(f.Data) != string(content) || f.Filename != tc.file || f.Mime != tc.mime || f.HTMLCaption != tc.caption {
				t.Errorf("file = %+v", f)
			}
			evs := w.events.Staffs()
			if len(evs) != 1 || evs[0].Text != tc.plain || evs[0].HubMsgID != "5550001111:7001" || evs[0].Source != "topic" {
				t.Errorf("staff events = %+v (a reply with no words must still be reported)", evs)
			}
			if _, _, found, _ := w.pairs.CustomerMsgFor(1250, 600); !found {
				t.Error("pair not recorded")
			}
		})
	}
}

func TestStaffFailuresAreReturnedAndNothingIsReported(t *testing.T) {
	text := func() *gotgbot.Message { m := staffMsg(700); m.Text = "hi"; return m }
	photo := func() *gotgbot.Message {
		m := staffMsg(701)
		m.Photo = []gotgbot.PhotoSize{{FileId: "l", Width: 1, Height: 1}}
		return m
	}
	poll := func() *gotgbot.Message { m := staffMsg(702); m.Poll = &gotgbot.Poll{Id: "p"}; return m }

	for name, tc := range map[string]struct {
		msg    func() *gotgbot.Message
		setup  func(w *world)
		wantIs error
	}{
		"customer blocked the bot": {text, func(w *world) {
			w.sender.err = fmt.Errorf("%w: Forbidden", agentlink.ErrCustomerBlocked)
		}, agentlink.ErrCustomerBlocked},
		"unsupported kind": {poll, func(*world) {}, ErrUnsupported},
		"file too big for the bot": {photo, func(w *world) {
			w.topics.dlErr["l"] = ErrTooBig
		}, ErrTooBig},
		"file cannot be downloaded": {photo, func(w *world) {
			w.topics.dlErr["l"] = errors.New("boom")
		}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(w)
			err := w.bridge.StaffToCustomer(t.Context(), tc.msg(), customerID)
			if err == nil || (tc.wantIs != nil && !errors.Is(err, tc.wantIs)) {
				t.Fatalf("err = %v", err)
			}
			if len(w.events.Staffs()) != 0 {
				t.Error("a failed send was reported as staff speech")
			}
			if _, _, found, _ := w.pairs.CustomerMsgFor(1250, tc.msg().MessageId); found {
				t.Error("a pair was recorded for a failed send")
			}
		})
	}
}

func TestIsServiceMessage(t *testing.T) {
	for name, m := range map[string]*gotgbot.Message{
		"topic created": {ForumTopicCreated: &gotgbot.ForumTopicCreated{}},
		"topic edited":  {ForumTopicEdited: &gotgbot.ForumTopicEdited{}},
		"pin":           {PinnedMessage: &gotgbot.Message{}},
		"member joins":  {NewChatMembers: []gotgbot.User{{Id: 1}}},
		"title":         {NewChatTitle: "x"},
	} {
		if !IsServiceMessage(m) {
			t.Errorf("%s is not recognised as a service message", name)
		}
	}
	for name, m := range map[string]*gotgbot.Message{
		"text":    {Text: "hi"},
		"photo":   {Photo: []gotgbot.PhotoSize{{FileId: "x"}}},
		"poll":    {Poll: &gotgbot.Poll{}},
		"nothing": {},
	} {
		if IsServiceMessage(m) {
			t.Errorf("%s is taken for a service message", name)
		}
	}
}
