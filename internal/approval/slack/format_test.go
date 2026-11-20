package slack

import (
	"fmt"
	"strings"
	"testing"
)

const formatTestBotToken = "xoxb-format-test-secret-token-do-not-leak"

func TestSlackValuesFormattingRedactsBotToken(t *testing.T) {
	client, err := NewClient(ClientOptions{
		BaseURL:  "https://slack.example.test/api",
		BotToken: formatTestBotToken,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	options := ClientOptions{
		BaseURL:  "https://slack.example.test/api",
		BotToken: formatTestBotToken,
	}
	handlers := &Handlers{client: client, channelID: "C123"}
	interaction := &InteractionHandler{client: client}
	channel := &Channel{client: client, channelID: "C123"}

	targets := []struct {
		name         string
		value        any
		wantRedacted bool
	}{
		{name: "client value", value: *client, wantRedacted: true},
		{name: "client pointer", value: client, wantRedacted: true},
		{name: "client options value", value: options, wantRedacted: true},
		{name: "client options pointer", value: &options, wantRedacted: true},
		{name: "handlers value", value: *handlers},
		{name: "handlers pointer", value: handlers},
		{name: "interaction handler value", value: *interaction},
		{name: "interaction handler pointer", value: interaction},
		{name: "channel value", value: *channel},
		{name: "channel pointer", value: channel},
	}
	verbs := []string{"%v", "%+v", "%#v", "%s"}
	for _, target := range targets {
		for _, verb := range verbs {
			t.Run(target.name+"/"+verb, func(t *testing.T) {
				rendered := fmt.Sprintf(verb, target.value)
				if strings.Contains(rendered, formatTestBotToken) {
					t.Fatalf("%s leaked the bot token: %q", verb, rendered)
				}
				if strings.Contains(rendered, "Bearer") {
					t.Fatalf("%s leaked an authorization header: %q", verb, rendered)
				}
				if target.wantRedacted && !strings.Contains(rendered, redactedSecret) {
					t.Fatalf("%s = %q, want %q", verb, rendered, redactedSecret)
				}
			})
		}
	}
}
