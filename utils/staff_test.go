package utils

import (
	"errors"
	"testing"
	"time"

	"watgbridge/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestIsStaffUserID(t *testing.T) {
	cfg := state.State.Config
	oldOwner, oldSudo, oldChat, oldFetch := cfg.Telegram.OwnerID, cfg.Telegram.SudoUsersID, cfg.Telegram.TargetChatID, fetchGroupAdmins
	t.Cleanup(func() {
		cfg.Telegram.OwnerID, cfg.Telegram.SudoUsersID, cfg.Telegram.TargetChatID, fetchGroupAdmins = oldOwner, oldSudo, oldChat, oldFetch
		groupAdmins.chatID, groupAdmins.ids, groupAdmins.nextFetch = 0, nil, time.Time{}
	})
	cfg.Telegram.OwnerID, cfg.Telegram.SudoUsersID, cfg.Telegram.TargetChatID = 1, []int64{2}, -1001
	groupAdmins.chatID, groupAdmins.ids, groupAdmins.nextFetch = 0, nil, time.Time{}

	calls := 0
	admins, fail := []int64{3}, false
	fetchGroupAdmins = func(_ *gotgbot.Bot, chatID int64) ([]int64, error) {
		calls++
		if chatID != -1001 {
			t.Errorf("fetched admins of %d, want the staff group", chatID)
		}
		if fail {
			return nil, errors.New("telegram down")
		}
		return admins, nil
	}

	for id, want := range map[int64]bool{1: true, 2: true, 3: true, 4: false} {
		if got := IsStaffUserID(nil, id); got != want {
			t.Errorf("user %d: got %v, want %v", id, got, want)
		}
	}
	if calls != 1 {
		t.Errorf("admin list fetched %d times, want once (owner and sudo need no lookup, the list is cached)", calls)
	}

	// Past the TTL a failed refresh keeps the last list and backs off.
	groupAdmins.nextFetch = time.Now().Add(-time.Second)
	fail = true
	if !IsStaffUserID(nil, 3) {
		t.Error("an admin was locked out by a failed refresh")
	}
	if IsStaffUserID(nil, 3); calls != 2 {
		t.Errorf("a failed refresh was retried at once (%d fetches)", calls)
	}

	// A refresh that succeeds drops a demoted admin.
	groupAdmins.nextFetch = time.Now().Add(-time.Second)
	fail, admins = false, []int64{5}
	if IsStaffUserID(nil, 3) || !IsStaffUserID(nil, 5) {
		t.Error("the refreshed admin list was not used")
	}

	// No staff group configured: only owner and sudo, no lookup.
	cfg.Telegram.TargetChatID = 0
	before := calls
	if IsStaffUserID(nil, 5) || calls != before {
		t.Error("admins were looked up with no staff group configured")
	}
}
