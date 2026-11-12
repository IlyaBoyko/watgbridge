package utils

import (
	"context"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waTypes "go.mau.fi/whatsmeow/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"watgbridge/database"
	"watgbridge/state"
)

const (
	testLID   = "99887766@lid"
	testPhone = "60123456789@s.whatsapp.net"
)

func usernameEnv(t *testing.T) {
	t.Helper()
	container, err := sqlstore.New(context.Background(), "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "wa.db")+"?_foreign_keys=on", nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := container.NewDevice()
	own := waTypes.NewJID("60100000000", waTypes.DefaultUserServer)
	dev.ID = &own
	dev.LIDs = container.LIDMap
	client := whatsmeow.NewClient(dev, nil)

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	prevDB, prevClient := state.State.Database, state.State.WhatsAppClient
	t.Cleanup(func() {
		state.State.Database, state.State.WhatsAppClient = prevDB, prevClient
		sqlDB.Close()
	})
	state.State.Database, state.State.WhatsAppClient = db, client
	if err := database.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
}

func mustJID(t *testing.T, s string) waTypes.JID {
	t.Helper()
	j, err := waTypes.ParseJID(s)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestWaGetContactUsername(t *testing.T) {
	usernameEnv(t)
	lid, phone := mustJID(t, testLID), mustJID(t, testPhone)

	if got := WaGetContactUsername(lid); got != "" {
		t.Fatalf("nothing stored: %q", got)
	}
	if err := database.ContactUpdateUsername(lid.User, lid.Server, "hanifmahyudin"); err != nil {
		t.Fatal(err)
	}
	if got := WaGetContactUsername(lid); got != "hanifmahyudin" {
		t.Fatalf("LID chat: %q", got)
	}
	// The phone has no mapping to the LID yet, so it finds nothing.
	if got := WaGetContactUsername(phone); got != "" {
		t.Fatalf("unmapped phone: %q", got)
	}
	if err := state.State.WhatsAppClient.Store.LIDs.PutLIDMapping(context.Background(), lid, phone); err != nil {
		t.Fatal(err)
	}
	if got := WaGetContactUsername(phone); got != "hanifmahyudin" {
		t.Fatalf("phone chat whose LID has a username: %q", got)
	}
}

func TestWaGetContactUsernameStripsAt(t *testing.T) {
	usernameEnv(t)
	lid := mustJID(t, testLID)
	if err := database.ContactUpdateUsername(lid.User, lid.Server, "@hanif"); err != nil {
		t.Fatal(err)
	}
	if got := WaGetContactUsername(lid); got != "hanif" {
		t.Fatalf("got %q", got)
	}
}

func TestWaGetContactNameKeepsItsTags(t *testing.T) {
	usernameEnv(t)
	lid, phone := mustJID(t, testLID), mustJID(t, testPhone)
	if err := database.ContactNameAddNew(lid.User, lid.Server, "Hanif", "Hanif Mahyudin", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.ContactUpdateUsername(lid.User, lid.Server, "hanifmahyudin"); err != nil {
		t.Fatal(err)
	}
	if got := WaGetContactName(lid); got != "Hanif Mahyudin (@hanifmahyudin)" {
		t.Fatalf("hidden number: %q", got)
	}

	if err := database.ContactNameAddNew(phone.User, phone.Server, "Aisha", "Aisha Rahman", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := WaGetContactName(phone); got != "Aisha Rahman (60123456789)" {
		t.Fatalf("phone number: %q", got)
	}
}
