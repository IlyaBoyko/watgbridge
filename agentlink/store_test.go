package agentlink

import (
	"testing"
	"time"

	"watgbridge/database"
)

// The link's tables are new; the bridge's existing rows must be untouched, and
// migrating again over a populated database must be harmless.
func TestMigrateOnPopulatedDatabase(t *testing.T) {
	w := newWorld(t)
	if err := w.db.AutoMigrate(&database.MsgIdPair{}, &database.ChatThreadPair{}); err != nil {
		t.Fatal(err)
	}
	w.db.Create(&database.ChatThreadPair{ID: "60123456789@s.whatsapp.net", TgChatId: -100123, TgThreadId: 1234})
	w.db.Create(&database.MsgIdPair{ID: "3EB0A1", ParticipantId: "60123456789@s.whatsapp.net", WaChatId: "60123456789@s.whatsapp.net", TgChatId: -100123, TgMsgId: 55, TgThreadId: 1234})

	if err := Migrate(w.db); err != nil {
		t.Fatal(err)
	}
	ob := NewOutbox(w.db, w.clock)
	if _, err := ob.Add("wa-1", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := NewCommandMemory(w.db, w.clock).Put(Result{CommandID: "C1", OK: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := NewCardStore(w.db, w.clock).Create(CardRow{CardID: "draft_1", Conversation: "wa:60123456789@s.whatsapp.net"}); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(w.db); err != nil { // a restart
		t.Fatal(err)
	}
	if _, ok, _ := NewCardStore(w.db, w.clock).ByCardID("draft_1"); !ok {
		t.Error("card lost by re-migrate")
	}

	var threads []database.ChatThreadPair
	var pairs []database.MsgIdPair
	w.db.Find(&threads)
	w.db.Find(&pairs)
	if len(threads) != 1 || threads[0].TgThreadId != 1234 || len(pairs) != 1 || pairs[0].TgMsgId != 55 {
		t.Errorf("existing bridge rows changed: %+v %+v", threads, pairs)
	}
	if n, _ := ob.Count(); n != 1 {
		t.Errorf("outbox rows after re-migrate = %d, want 1", n)
	}
	if _, ok, _ := NewCommandMemory(w.db, w.clock).Get("C1"); !ok {
		t.Error("command result lost by re-migrate")
	}
}

func TestOutboxOrderingAndDedupe(t *testing.T) {
	w := newWorld(t)
	if err := Migrate(w.db); err != nil {
		t.Fatal(err)
	}
	ob := NewOutbox(w.db, w.clock)
	for _, id := range []string{"b", "a", "c"} {
		if added, err := ob.Add(id, []byte(`{"id":"`+id+`"}`)); err != nil || !added {
			t.Fatalf("add %s: %v %v", id, added, err)
		}
	}
	if added, err := ob.Add("a", []byte(`{"id":"a2"}`)); err != nil || added {
		t.Fatalf("duplicate add: added=%v err=%v", added, err)
	}
	rows, _ := ob.Pending(0, 10)
	if len(rows) != 3 || rows[0].EventID != "b" || rows[1].EventID != "a" || rows[2].EventID != "c" {
		t.Fatalf("order = %+v", rows)
	}
	if rows[1].JSON != `{"id":"a"}` {
		t.Errorf("a duplicate overwrote the queued event: %s", rows[1].JSON)
	}
	after, _ := ob.Pending(rows[0].Seq, 10)
	if len(after) != 2 || after[0].EventID != "a" {
		t.Errorf("Pending(after) = %+v", after)
	}
	_ = ob.Delete("a")
	if n, _ := ob.Count(); n != 2 {
		t.Errorf("count after delete = %d", n)
	}
	// A deleted id's sequence number is never reused.
	_, _ = ob.Add("d", []byte(`{}`))
	rows, _ = ob.Pending(0, 10)
	if rows[len(rows)-1].EventID != "d" || rows[len(rows)-1].Seq <= rows[len(rows)-2].Seq {
		t.Errorf("sequence went backwards: %+v", rows)
	}
}

// A3: outbox_max_age_days drops old rows at startup, with a warning.
func TestOutboxMaxAgeDropsOldRowsAtStartup(t *testing.T) {
	w := newWorld(t)
	if err := Migrate(w.db); err != nil {
		t.Fatal(err)
	}
	ob := NewOutbox(w.db, w.clock)
	_, _ = ob.Add("ancient", []byte(`{}`))
	w.clock.Advance(6 * 24 * time.Hour)
	_, _ = ob.Add("recent", []byte(`{}`))
	w.clock.Advance(25 * time.Hour) // ancient is 7d1h old, recent 25h

	h, err := NewHub(Config{Enabled: true, URL: "ws://x", Token: "t", HubID: "h", OutboxMaxAgeDays: 7}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := h.outbox.Pending(0, 10)
	if len(rows) != 1 || rows[0].EventID != "recent" {
		t.Fatalf("rows after startup = %+v", rows)
	}

	// 0 keeps everything (protocol section 7: an old event is still evidence).
	w.clock.Advance(365 * 24 * time.Hour)
	h2, err := NewHub(Config{Enabled: true, URL: "ws://x", Token: "t", HubID: "h", OutboxMaxAgeDays: 0}, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := h2.outbox.Count(); n != 1 {
		t.Errorf("max age 0 dropped rows: %d left", n)
	}
}
