package agentlink

import "github.com/PaulSonOfLars/gotgbot/v2"

// menuCommands is the single source for the agent's entries in Telegram's "/"
// menu. They are only shown there: the /ai_ interceptor (ParseAICommand) takes
// them in topics and nothing here handles them.
var menuCommands = []gotgbot.BotCommand{
	{Command: "ai_status", Description: "Show the agent mode for this chat"},
	{Command: "ai_on", Description: "Agent answers this chat by itself"},
	{Command: "ai_draft", Description: "Agent posts replies here as cards to send"},
	{Command: "ai_after", Description: "Cards send themselves after N minutes, e.g. /ai_after 10"},
	{Command: "ai_off", Description: "Agent stays silent in this chat"},
	{Command: "ai_once", Description: "One-off instruction for the next reply: /ai_once <text>"},
	{Command: "ai_slip", Description: "Send the customer's latest slip for checking: /ai_slip <order ref>"},
}

// MenuCommands returns a copy of the agent's menu entries.
func MenuCommands() []gotgbot.BotCommand {
	return append([]gotgbot.BotCommand(nil), menuCommands...)
}

// WithMenuCommands returns base followed by the agent's menu entries when the
// agent is enabled, and base unchanged otherwise. base is never modified.
func WithMenuCommands(base []gotgbot.BotCommand, agentEnabled bool) []gotgbot.BotCommand {
	out := append([]gotgbot.BotCommand(nil), base...)
	if agentEnabled {
		out = append(out, menuCommands...)
	}
	return out
}
