package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// DiscordNotifier delivers messages to a Discord Webhook.
type DiscordNotifier struct {
	WebhookURL string
	http       *http.Client
}

func NewDiscord(webhookURL string) *DiscordNotifier {
	return &DiscordNotifier{
		WebhookURL: webhookURL,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func (n *DiscordNotifier) Name() string {
	return "discord-webhook"
}

// discordContentLimit is Discord's hard 2000-character webhook content cap.
const discordContentLimit = 2000

func (n *DiscordNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	content := msg.Text
	if len([]rune(content)) > discordContentLimit {
		// Truncate on rune boundary; reserve 3 chars for ellipsis.
		runes := []rune(content)
		content = string(runes[:discordContentLimit-3]) + "..."
	}
	payload := map[string]interface{}{
		"content": content,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", n.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord api error (%d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}
