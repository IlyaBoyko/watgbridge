package lzcustomer_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"watgbridge/agentlink"
	. "watgbridge/lzcustomer"
	"watgbridge/lzcustomer/lzfake"
)

func eventTexts(w *world) []string {
	var out []string
	for _, e := range w.events.Customers() {
		out = append(out, e.Text)
	}
	return out
}

// Every template of the table in the brief, with the shapes real traffic may
// not have: missing fields, numbers for ids, content that is not JSON.
func TestTemplatesBecomeTextMarkers(t *testing.T) {
	cases := []struct {
		name     string
		template int
		content  string
		want     string
	}{
		{"text", 1, `{"txt":"hi there"}`, "hi there"},
		{"text not json", 1, `plain words`, "plain words"},
		{"text without txt", 1, `{}`, "[message type 1]"},
		{"emoji", 4, `{"txt":"😀"}`, "😀"},
		{"emoji by another key", 4, `{"emoji":"👍"}`, "👍"},
		{"emoji empty", 4, `{}`, "[emoji]"},
		{"emoji garbage", 4, `not json`, "[emoji]"},
		{"image without url", 3, `{}`, "[image]"},
		{"video", 6, `{"videoUrl":"https://x"}`, "[video]"},
		{"item", 10006, `{"title":"BPC-157 5mg","itemId":123}`, "[product card: BPC-157 5mg (item 123)]"},
		{"item other keys", 10006, `{"itemTitle":"TB-500","item_id":"77"}`, "[product card: TB-500 (item 77)]"},
		{"item name only", 10006, `{"name":"GHK-Cu"}`, "[product card: GHK-Cu]"},
		{"item id only", 10006, `{"itemId":5}`, "[product card (item 5)]"},
		{"item empty", 10006, `{}`, "[product card]"},
		{"item garbage", 10006, `[1,2,`, "[product card]"},
		{"item odd types", 10006, `{"title":["x"],"itemId":{"a":1}}`, "[product card]"},
		{"item long id stays exact", 10006, `{"title":"A","itemId":1234567890123456789}`, "[product card: A (item 1234567890123456789)]"},
		{"order", 10007, `{"orderId":"9001"}`, "[order card: 9001]"},
		{"order number id", 10007, `{"order_id":9002}`, "[order card: 9002]"},
		{"order trade id", 10007, `{"tradeOrderId":"T3"}`, "[order card: T3]"},
		{"order empty", 10007, `{}`, "[order card]"},
		{"voucher", 10008, `{"voucherId":"v"}`, "[voucher]"},
		{"unknown", 99, `{}`, "[message type 99]"},
	}
	w := newWorld(t)
	w.start()
	for _, c := range cases {
		w.clock.Advance(10 * time.Millisecond)
		w.lz.AddBuyerMessage("s1", c.template, c.content)
	}
	mustNoErr(t, w.scan())

	got := eventTexts(w)
	if len(got) != len(cases) {
		t.Fatalf("%d events for %d messages: %q", len(got), len(cases), got)
	}
	topic := w.topics.texts(1250)
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("%s: event text %q, want %q", c.name, got[i], c.want)
		}
		// What staff see in the topic is the same words.
		if topic[i] != c.want {
			t.Errorf("%s: topic text %q, want %q", c.name, topic[i], c.want)
		}
	}
}

// Content key names are logged once per template id, never their values.
func TestContentKeysAreLoggedOncePerTemplateWithoutValues(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.lz.AddBuyerMessage("s1", 10006, `{"title":"SECRET-TITLE","itemId":1,"extra":"x"}`)
	w.clock.Advance(10 * time.Millisecond)
	w.lz.AddBuyerMessage("s1", 10006, `{"title":"OTHER","zzz":true}`)
	w.clock.Advance(10 * time.Millisecond)
	w.lz.AddBuyerMessage("s1", 10007, `{"orderId":"123"}`)
	w.clock.Advance(10 * time.Millisecond)
	w.buyer("s1", "plain text")
	mustNoErr(t, w.scan())
	mustNoErr(t, w.scan())

	got := w.logged("lazada: content keys")
	want := []string{
		"lazada: content keys for template 10006: [extra itemId title]",
		"lazada: content keys for template 10007: [orderId]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("logged %q, want %q", got, want)
	}
	for _, e := range w.logs.All() {
		if line := fmt.Sprint(e.Message, e.Context); strings.Contains(line, "SECRET-TITLE") || strings.Contains(line, "plain text") {
			t.Errorf("a log line carries message content: %q", line)
		}
	}
}

func imageServer(t *testing.T, routes map[string]func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if h, ok := routes[r.URL.Path]; ok {
			h(rw)
			return
		}
		http.NotFound(rw, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serveImage(data []byte, mime string) func(http.ResponseWriter) {
	return func(rw http.ResponseWriter) {
		rw.Header().Set("Content-Type", mime)
		_, _ = rw.Write(data)
	}
}

// A picture is downloaded and goes to the topic as a photo and to the Agent as
// media[] of kind image. Any of the four URL keys works, in order of preference.
func TestImageIsDownloadedPostedAndSentAsMedia(t *testing.T) {
	png := []byte("\x89PNG-bytes")
	srv := imageServer(t, map[string]func(http.ResponseWriter){
		"/a.png": serveImage(png, "image/png"),
		"/b.jpg": serveImage([]byte("JPEG"), "image/jpeg; charset=binary"),
	})
	w := newWorld(t, func(o *worldOpts) { o.images = NewHTTPImages(srv.Client(), true) })
	w.start()
	w.clock.Advance(10 * time.Millisecond)
	w.lz.AddBuyerMessage("s1", 3, fmt.Sprintf(`{"url":%q,"imgUrl":%q}`, srv.URL+"/ignored", srv.URL+"/a.png"))
	for _, key := range []string{"img_url", "url", "originUrl"} {
		w.clock.Advance(10 * time.Millisecond)
		w.lz.AddBuyerMessage("s1", 3, fmt.Sprintf(`{%q:%q}`, key, srv.URL+"/b.jpg"))
	}
	mustNoErr(t, w.scan())

	evs := w.events.Customers()
	if len(evs) != 4 {
		t.Fatalf("%d events", len(evs))
	}
	if evs[0].Text != "" || len(evs[0].Media) != 1 {
		t.Fatalf("image event = %+v", evs[0])
	}
	m := evs[0].Media[0]
	if m.Kind != "image" || m.Mime != "image/png" || m.Size != int64(len(png)) || m.Data != base64.StdEncoding.EncodeToString(png) || m.TooLarge || m.Filename != "image.png" {
		t.Errorf("media = %+v", m)
	}
	var photos []fakePost
	for _, p := range w.topics.Posts() {
		if p.Kind == "photo" {
			photos = append(photos, p)
		}
	}
	if len(photos) != 4 || string(photos[0].Data) != string(png) {
		t.Fatalf("%d photo posts", len(photos))
	}
	for i := 1; i < 4; i++ {
		if evs[i].Media[0].Mime != "image/jpeg" || string(photos[i].Data) != "JPEG" {
			t.Errorf("image %d (key variant) = %+v", i, evs[i].Media)
		}
	}
}

// No URL, a failed download, a non-image answer and an oversized file all fall
// back to the text [image].
func TestImageFailuresFallBackToText(t *testing.T) {
	big := make([]byte, 10<<20+1)
	srv := imageServer(t, map[string]func(http.ResponseWriter){
		"/html": serveImage([]byte("<html>"), "text/html"),
		"/big":  serveImage(big, "image/jpeg"),
		"/gone": func(rw http.ResponseWriter) { http.Error(rw, "no", http.StatusGone) },
	})
	w := newWorld(t, func(o *worldOpts) { o.images = NewHTTPImages(srv.Client(), true) })
	w.start()
	contents := []string{
		`{}`,
		fmt.Sprintf(`{"imgUrl":%q}`, srv.URL+"/missing"),
		fmt.Sprintf(`{"imgUrl":%q}`, srv.URL+"/gone"),
		fmt.Sprintf(`{"imgUrl":%q}`, srv.URL+"/html"),
		fmt.Sprintf(`{"imgUrl":%q}`, srv.URL+"/big"),
		`{"imgUrl":"ftp://example.invalid/a.png"}`,
		`{"imgUrl":"https://127.0.0.1:1/unreachable.png"}`,
	}
	for _, c := range contents {
		w.clock.Advance(10 * time.Millisecond)
		w.lz.AddBuyerMessage("s1", 3, c)
	}
	mustNoErr(t, w.scan())
	got := eventTexts(w)
	if len(got) != len(contents) {
		t.Fatalf("%d events: %q", len(got), got)
	}
	for i, g := range got {
		if g != "[image]" {
			t.Errorf("case %d (%s): text %q, want [image]", i, contents[i], g)
		}
	}
	for _, e := range w.events.Customers() {
		if len(e.Media) != 0 {
			t.Errorf("media on a failed download: %+v", e.Media)
		}
	}
	for _, e := range w.logs.All() {
		if strings.Contains(fmt.Sprint(e.Message, e.Context), srv.URL) {
			t.Errorf("a log line carries the picture's address: %v", e)
		}
	}
}

// A picture over the inline limit is shown in the topic and described to the
// Agent without its data (protocol section 6).
func TestLargeImageIsPostedButNotInlined(t *testing.T) {
	data := make([]byte, agentlink.MaxInlineMediaBytes+1)
	srv := imageServer(t, map[string]func(http.ResponseWriter){"/l.jpg": serveImage(data, "image/jpeg")})
	w := newWorld(t, func(o *worldOpts) { o.images = NewHTTPImages(srv.Client(), true) })
	w.start()
	w.clock.Advance(10 * time.Millisecond)
	w.lz.AddBuyerMessage("s1", 3, fmt.Sprintf(`{"imgUrl":%q}`, srv.URL+"/l.jpg"))
	mustNoErr(t, w.scan())
	m := w.events.Customers()[0].Media[0]
	if !m.TooLarge || m.Data != "" || m.Size != int64(len(data)) {
		t.Errorf("media = %+v", m)
	}
	if p := w.topics.Posts()[0]; p.Kind != "photo" || len(p.Data) != len(data) {
		t.Errorf("topic post = %q (%d bytes)", p.Kind, len(p.Data))
	}
}

// A photo Telegram refuses goes up as a file instead.
func TestPhotoRefusedByTelegramIsPostedAsADocument(t *testing.T) {
	srv := imageServer(t, map[string]func(http.ResponseWriter){"/p.png": serveImage([]byte("PNG"), "image/png")})
	w := newWorld(t, func(o *worldOpts) { o.images = NewHTTPImages(srv.Client(), true) })
	w.topics.postErr = func(p TopicPost) error {
		if p.Kind == "photo" {
			return fmt.Errorf("PHOTO_INVALID_DIMENSIONS")
		}
		return nil
	}
	w.start()
	w.clock.Advance(10 * time.Millisecond)
	w.lz.AddBuyerMessage("s1", 3, fmt.Sprintf(`{"imgUrl":%q}`, srv.URL+"/p.png"))
	mustNoErr(t, w.scan())
	posts := w.topics.Posts()
	if len(posts) != 1 || posts[0].Kind != "document" {
		t.Errorf("posts = %+v", posts)
	}
	if len(w.events.Customers()) != 1 {
		t.Error("the event was not sent")
	}
}

// The downloader itself: https only unless told otherwise.
func TestHTTPImagesRefusesPlainHTTPUnlessAllowed(t *testing.T) {
	srv := imageServer(t, map[string]func(http.ResponseWriter){"/a.png": serveImage([]byte("PNG"), "image/png")})
	strict := NewHTTPImages(srv.Client(), false)
	if _, _, err := strict.Fetch(t.Context(), srv.URL+"/a.png"); err == nil {
		t.Error("a plain http address was fetched")
	}
	if _, _, err := NewHTTPImages(srv.Client(), true).Fetch(t.Context(), srv.URL+"/a.png"); err != nil {
		t.Errorf("allowed http failed: %v", err)
	}
}

// A buyer who takes a message back leaves a note and no event.
func TestRecalledBuyerMessageIsANoteWithoutAnEvent(t *testing.T) {
	w := newWorld(t)
	w.start()
	id := w.buyer("s1", "oops wrong chat")
	w.lz.RecallMessage("s1", id)
	mustNoErr(t, w.scan())
	if n := len(w.events.Customers()); n != 0 {
		t.Errorf("%d events for a recalled message", n)
	}
	if got := w.topics.texts(1250); !reflect.DeepEqual(got, []string{"ℹ️ The buyer recalled a message."}) {
		t.Errorf("topic = %q", got)
	}
	if c, _, _ := w.svc.Cursor("s1"); c.LastMessageID != id {
		t.Errorf("cursor = %+v", c)
	}
}

// System messages are notes: their text, or a stand-in.
func TestSystemMessagesAreNotes(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.clock.Advance(10 * time.Millisecond)
	w.lz.AddSellerMessage("s1", 1, `{"txt":"Order 123 was shipped"}`, lzfake.SellerOpts{System: true})
	w.clock.Advance(10 * time.Millisecond)
	w.lz.AddSellerMessage("s1", 10007, `{}`, lzfake.SellerOpts{System: true})
	mustNoErr(t, w.scan())
	want := []string{"ℹ️ Order 123 was shipped", "ℹ️ [Lazada system message]"}
	if got := w.topics.texts(1250); !reflect.DeepEqual(got, want) {
		t.Errorf("topic = %q, want %q", got, want)
	}
	if len(w.events.Customers())+len(w.events.Staffs()) != 0 {
		t.Error("a system message produced an event")
	}
}

// Staff replying from Seller Center is the same case as the WhatsApp phone:
// mirrored in the topic, and a staff.message with source phone.
func TestSellerCenterReplyIsMirroredAndReportedAsPhone(t *testing.T) {
	w := newWorld(t)
	w.start()
	buyerID := w.buyer("s1", "is it in stock?")
	id := w.seller("s1", "Yes, shipping today", lzfake.SellerOpts{})
	mustNoErr(t, w.scan())

	want := []string{
		"create-topic:Lazada · Buyer s1",
		"post:1250:text:is it in stock?",
		"customer:" + buyerID,
		"post:1250:text:🧑: You [Seller Center]|Yes, shipping today",
		"staff:" + id,
	}
	if got := w.j.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %q, want %q", got, want)
	}
	st := w.events.Staffs()[0]
	if st.Source != "phone" || st.HubMsgID != id || st.ChatKey != "lz:s1" || st.Text != "Yes, shipping today" || st.TopicID != 1250 || st.TgUserID != 0 {
		t.Errorf("staff event = %+v", st)
	}
}

// Lazada's own auto-reply is a note: never a staff takeover.
func TestAutoReplyIsANoteOnly(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.seller("s1", "Thanks for contacting us!", lzfake.SellerOpts{AutoReply: true})
	mustNoErr(t, w.scan())
	if got := w.topics.texts(1250); !reflect.DeepEqual(got, []string{"ℹ️ Lazada auto-reply: Thanks for contacting us!"}) {
		t.Errorf("topic = %q", got)
	}
	if len(w.events.Staffs()) != 0 {
		t.Error("an auto-reply was reported as staff speech")
	}
}

// A message the Hub sent was mirrored and reported when it went out: the
// fetcher stays silent about it, but Lazada's verdict (A6) is still new.
func TestMessagesTheHubSentAreSkippedButBlockedOnesAreWarnedAbout(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.buyer("s1", "hello")
	mustNoErr(t, w.scan())
	w.j.lines = nil

	fine := w.seller("s1", "our reply", lzfake.SellerOpts{})
	blocked := w.seller("s1", "see wa.me/60123", lzfake.SellerOpts{ProcessMsg: "contains contact information"})
	mustNoErr(t, w.svc.RecordSent("s1", fine))
	mustNoErr(t, w.svc.RecordSent("s1", blocked))
	mustNoErr(t, w.scan())

	want := []string{"post:1250:text:⚠️ Lazada blocked this message: contains contact information"}
	if got := w.j.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("journal = %q, want %q", got, want)
	}
	if c, _, _ := w.svc.Cursor("s1"); c.LastMessageID != blocked {
		t.Errorf("the cursor did not pass the skipped messages: %+v", c)
	}
}

// Skipping a sent message makes no topic: there is nothing to show in it.
func TestSkippedSentMessageDoesNotCreateATopic(t *testing.T) {
	w := newWorld(t)
	w.start()
	id := w.seller("s1", "our reply", lzfake.SellerOpts{})
	mustNoErr(t, w.svc.RecordSent("s1", id))
	mustNoErr(t, w.scan())
	if len(w.topics.Created()) != 0 || len(w.j.all()) != 0 {
		t.Errorf("journal = %q", w.j.all())
	}
}

// A staff reply Lazada blocked: mirror, event and the warning.
func TestSellerCenterReplyThatWasBlockedAlsoGetsTheWarning(t *testing.T) {
	w := newWorld(t)
	w.start()
	id := w.seller("s1", "call me", lzfake.SellerOpts{ProcessMsg: "phone number"})
	mustNoErr(t, w.scan())
	want := []string{
		"create-topic:Lazada · Buyer s1",
		"post:1250:text:🧑: You [Seller Center]|call me",
		"post:1250:text:⚠️ Lazada blocked this message: phone number",
		"staff:" + id,
	}
	if got := w.j.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("journal = %q, want %q", got, want)
	}
}

func TestBuyerTextIsBoldOnlyWhenConfigured(t *testing.T) {
	for _, bold := range []bool{false, true} {
		w := newWorld(t, func(o *worldOpts) { o.boldBuyer = bold })
		w.start()
		w.buyer("s1", "hi")
		w.seller("s1", "hello", lzfake.SellerOpts{})
		mustNoErr(t, w.scan())
		posts := w.topics.Posts()
		if posts[0].Bold != bold || posts[1].Bold {
			t.Errorf("bold=%v: buyer post bold %v, staff mirror bold %v", bold, posts[0].Bold, posts[1].Bold)
		}
	}
}

func TestTopicNameIsCutToTelegramsLimit(t *testing.T) {
	w := newWorld(t)
	w.lz.AddSession("s1", "b1", strings.Repeat("é", 300))
	w.start()
	w.buyer("s1", "hi")
	mustNoErr(t, w.scan())
	name := w.topics.Created()[0]
	if n := len([]rune(name)); n != 128 || !strings.HasPrefix(name, "Lazada · é") {
		t.Errorf("topic name has %d runes: %q", n, name)
	}
}
