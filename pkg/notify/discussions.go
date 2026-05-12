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

// DiscussionsNotifier delivers messages to the api.txid.uk discussions system.
type DiscussionsNotifier struct {
	APIURL     string
	AuthToken  string // txid_session cookie value
	CSRFToken  string // X-CSRF-Token header
	SourceSite string // e.g., "news"
	Origin     string // e.g., "https://news.txid.uk"
	http       *http.Client
}

func NewDiscussions(apiURL, authToken, csrfToken, sourceSite, origin string) *DiscussionsNotifier {
	return &DiscussionsNotifier{
		APIURL:     apiURL,
		AuthToken:  authToken,
		CSRFToken:  csrfToken,
		SourceSite: sourceSite,
		Origin:     origin,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func (n *DiscussionsNotifier) Name() string {
	return "discussions"
}

// Send posts a message to the discussions API. The recipient parameter
// is the source URL of the content being commented on. The API decides
// internally whether this becomes a new discussion or a comment on an
// existing one based on source_url uniqueness.
func (n *DiscussionsNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	payload := map[string]interface{}{
		"source_url":  recipient,
		"source_site": n.SourceSite,
		"body":        msg.Text,
		"lang":        "en",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", n.APIURL+"/discussions", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "txid_session="+n.AuthToken)
	req.Header.Set("X-CSRF-Token", n.CSRFToken)
	req.Header.Set("Origin", n.Origin)

	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discussions api error (%d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}
