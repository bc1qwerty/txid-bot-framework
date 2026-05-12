package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

const bandPostURL = "https://openapi.band.us/v2.2/band/post/create"

// BandNotifier delivers messages to Naver Band via Open API.
type BandNotifier struct {
	AccessToken string
	BandKey     string
	http        *http.Client
}

func NewBand(accessToken, bandKey string) *BandNotifier {
	return &BandNotifier{
		AccessToken: accessToken,
		BandKey:     bandKey,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
}

func (n *BandNotifier) Name() string {
	return "naver-band"
}

func (n *BandNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	// Band cannot parse HTML/Markdown — prefer the plain-text variant when
	// a Formatter provided one. Recipient is ignored because BandKey
	// (passed to NewBand) already pins the destination Band.
	content := msg.PlainText
	if content == "" {
		content = msg.Text
	}

	params := url.Values{
		"access_token": {n.AccessToken},
		"band_key":     {n.BandKey},
		"content":      {content},
		"do_push":      {"true"},
	}

	resp, err := n.http.PostForm(bandPostURL, params)
	if err != nil {
		return fmt.Errorf("band post failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("band api error (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		ResultCode int `json:"result_code"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("band response parse failed: %w", err)
	}

	if result.ResultCode != 1 {
		return fmt.Errorf("band api business error: %s", string(body))
	}

	return nil
}
