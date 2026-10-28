package lzcustomer_test

import (
	"testing"
	"time"

	. "watgbridge/lzcustomer"
)

// Migrate runs at every start: on a database that already holds the bridge's
// own rows and this package's, it changes nothing.
func TestMigrateIsIdempotentOnAPopulatedDatabase(t *testing.T) {
	db := newTestDB(t)
	// What a live Pi has: rows in the bridge's table (stand-in) and ours.
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS chat_thread_pairs (id integer primary key, wa_chat_id text, tg_thread_id integer)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO chat_thread_pairs (wa_chat_id, tg_thread_id) VALUES ('lz:s1', 1250)`).Error; err != nil {
		t.Fatal(err)
	}
	st := NewStore(db, nil)
	mustNoErr(t, st.RecordSent("s1", "m1", OriginAgent))
	mustNoErr(t, db.Save(&CursorRow{SessionID: "s1", LastMessageID: "m1", LastSendTime: 5, Title: "Aiman"}).Error)
	mustNoErr(t, db.Save(&MetaRow{ID: 1, StartedAt: 42, FullScanWatermark: 99, LastExpiryWarningDay: "2026-10-28"}).Error)

	mustNoErr(t, Migrate(db))
	mustNoErr(t, Migrate(db))

	var cur CursorRow
	mustNoErr(t, db.First(&cur, "session_id = ?", "s1").Error)
	var meta MetaRow
	mustNoErr(t, db.First(&meta, 1).Error)
	if cur.LastMessageID != "m1" || cur.Title != "Aiman" || meta.StartedAt != 42 || meta.FullScanWatermark != 99 || meta.LastExpiryWarningDay != "2026-10-28" {
		t.Errorf("rows changed: %+v %+v", cur, meta)
	}
	if sent, _ := st.WasSent("s1", "m1"); !sent {
		t.Error("the sent row was lost")
	}
	var n int64
	db.Raw(`SELECT COUNT(*) FROM chat_thread_pairs`).Scan(&n)
	if n != 1 {
		t.Errorf("the bridge's rows changed: %d", n)
	}
}

func TestSentRowsAreUniquePerMessageAndPrunedAfterThirtyDays(t *testing.T) {
	db := newTestDB(t)
	clock := newSimClock()
	st := NewStore(db, clock.Now)

	mustNoErr(t, st.RecordSent("s1", "m1", OriginAgent))
	mustNoErr(t, st.RecordSent("s1", "m1", OriginTopic)) // twice is once
	mustNoErr(t, st.RecordSent("s2", "m1", OriginTopic)) // same id, other session
	var n int64
	db.Model(&SentRow{}).Count(&n)
	if n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
	if sent, _ := st.WasSent("s1", "m1"); !sent {
		t.Error("not found")
	}
	if sent, _ := st.WasSent("s1", "m2"); sent {
		t.Error("found a message nobody recorded")
	}

	clock.Advance(29 * 24 * time.Hour)
	mustNoErr(t, st.RecordSent("s1", "m3", OriginAgent))
	clock.Advance(2 * 24 * time.Hour) // m1 rows are 31 days old, m3 is 2
	pruned, err := st.PruneSent()
	mustNoErr(t, err)
	if pruned != 2 {
		t.Errorf("pruned %d, want 2", pruned)
	}
	if sent, _ := st.WasSent("s1", "m3"); !sent {
		t.Error("a young row was pruned")
	}
	if sent, _ := st.WasSent("s1", "m1"); sent {
		t.Error("an old row was kept")
	}
}
