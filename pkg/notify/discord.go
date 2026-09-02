package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// 총 시도 횟수(최초 1 + 재시도 2)와 429 대기 상한.
// ⚠ 웹훅 POST 는 멱등이 아니다 — 성공했는데 응답을 못 받으면 같은 글이 두 번
// 갈 수 있다. 그래서 **전송 오류와 429·5xx 만** 재시도하고 횟수를 짧게 둔다.
const (
	discordRetryMax = 3
	discordMaxWait  = 30 * time.Second
)

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

	// ⚠ 상태 검사는 있었지만 **재시도가 없었다**. 디스코드 웹훅은 429 를 자주
	// 주고(채널당 5/5초) Retry-After 를 헤더로 알려 준다. 한 번 실패하면 그
	// 알림은 그대로 사라진다 — 텔레그램 쪽과 같은 구멍이었다.
	// 4xx(429 제외)는 우리 페이로드가 틀린 것이므로 즉시 포기한다.
	//
	// ⚠ 재시도할 때 본문을 손으로 다시 감쌀 필요는 없다. NewRequestWithContext 에
	// *bytes.Reader 를 주면 Go 가 req.GetBody 를 채우고 Transport 가 재전송 때
	// 그걸로 본문을 되살린다(재설정 코드를 넣었다가, 빼도 두 번째 요청 본문이
	// 그대로 가는 것을 실측하고 지웠다 — 없어도 되는데 필요해 보이는 코드다).
	var lastErr error
	for attempt := 1; attempt <= discordRetryMax; attempt++ {
		resp, err := n.http.Do(req)
		if err != nil {
			lastErr = err
			if attempt < discordRetryMax {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return lastErr
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 400 {
			return nil
		}
		lastErr = fmt.Errorf("discord api error (%d): %s", resp.StatusCode, string(respBody))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt == discordRetryMax {
			return lastErr
		}
		wait := time.Duration(attempt) * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
				wait = time.Duration(secs * float64(time.Second))
			}
		}
		if wait > discordMaxWait {
			wait = discordMaxWait
		}
		time.Sleep(wait)
	}
	return lastErr
}
