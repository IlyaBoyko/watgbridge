package utils

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"watgbridge/state"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Staff are the owner, the sudo users, and every admin of the staff group
// (target_chat_id). Admins are read from Telegram, so promoting someone in the
// group is all it takes to let them reply to customers and use the bridge and
// /ai_* commands; demoting them takes it away again within groupAdminsTTL.

const (
	groupAdminsTTL     = 5 * time.Minute
	groupAdminsRetry   = time.Minute
	groupAdminsTimeout = 5 * time.Second
)

// fetchGroupAdmins returns the user ids of a group's human admins. A variable
// so tests can stand in for Telegram.
var fetchGroupAdmins = func(b *gotgbot.Bot, chatID int64) ([]int64, error) {
	if b == nil {
		return nil, errors.New("no bot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), groupAdminsTimeout)
	defer cancel()
	members, err := b.GetChatAdministratorsWithContext(ctx, chatID, nil)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		// Bots, including the anonymous-admin stand-in, are never staff.
		if u := m.GetUser(); !u.IsBot {
			ids = append(ids, u.Id)
		}
	}
	return ids, nil
}

var groupAdmins struct {
	sync.Mutex
	chatID    int64
	ids       []int64
	nextFetch time.Time
}

// IsStaffUserID reports whether a Telegram user may use the bridge.
func IsStaffUserID(b *gotgbot.Bot, userID int64) bool {
	cfg := state.State.Config
	if userID == cfg.Telegram.OwnerID || slices.Contains(cfg.Telegram.SudoUsersID, userID) {
		return true
	}
	if cfg.Telegram.TargetChatID == 0 {
		return false
	}
	return slices.Contains(currentGroupAdmins(b, cfg.Telegram.TargetChatID), userID)
}

// currentGroupAdmins refreshes the list when it is older than groupAdminsTTL.
// A failed refresh keeps the last list it had and tries again after
// groupAdminsRetry, so a Telegram hiccup neither locks admins out nor turns
// every message into an API call.
func currentGroupAdmins(b *gotgbot.Bot, chatID int64) []int64 {
	groupAdmins.Lock()
	defer groupAdmins.Unlock()

	now := time.Now()
	if groupAdmins.chatID != chatID {
		groupAdmins.chatID, groupAdmins.ids, groupAdmins.nextFetch = chatID, nil, time.Time{}
	}
	if now.Before(groupAdmins.nextFetch) {
		return groupAdmins.ids
	}
	ids, err := fetchGroupAdmins(b, chatID)
	if err != nil {
		groupAdmins.nextFetch = now.Add(groupAdminsRetry)
		return groupAdmins.ids
	}
	groupAdmins.ids, groupAdmins.nextFetch = ids, now.Add(groupAdminsTTL)
	return ids
}
