// Example: minimal bot using the framework.
// Demonstrates how to wire a Source + Formatter + Notifier + Store + Runner.
package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/bot"
	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
	"github.com/bc1qwerty/txid-bot-framework/pkg/notify"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

// ── RSS Source ─────────────────────────────────────────

type RSSSource struct {
	feedURL string
	client  *http.Client
}

func NewRSSSource(url string) *RSSSource {
	return &RSSSource{
		feedURL: url,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *RSSSource) Name() string { return "rss" }

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

func (s *RSSSource) Fetch(ctx context.Context) ([]core.Item, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.feedURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var feed rssFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, err
	}

	items := make([]core.Item, 0, len(feed.Channel.Items))
	for _, it := range feed.Channel.Items {
		id := it.GUID
		if id == "" {
			id = it.Link
		}
		items = append(items, core.Item{
			ID:      id,
			Title:   it.Title,
			URL:     it.Link,
			Content: it.Description,
		})
	}
	return items, nil
}

// ── Formatter ──────────────────────────────────────────

type RSSFormatter struct{}

func (f *RSSFormatter) Format(item core.Item) core.Message {
	text := fmt.Sprintf("<b>%s</b>\n\n%s", item.Title, item.URL)
	return core.Message{
		Text:      text,
		ParseMode: "HTML",
	}
}

// ── Main ───────────────────────────────────────────────

func main() {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN env var required")
	}
	feedURL := os.Getenv("FEED_URL")
	if feedURL == "" {
		feedURL = "https://hnrss.org/frontpage"
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./minimal-bot.db"
	}

	// Open store
	st, err := store.Open(dbPath, "minimal")
	if err != nil {
		log.Fatalf("store open: %v", err)
	}
	defer st.Close()

	// Create telegram notifier
	tg, err := notify.NewTelegram(token)
	if err != nil {
		log.Fatalf("telegram init: %v", err)
	}

	// Start dispatcher for /start /subscribe /unsubscribe
	dispatcher := bot.NewTelegramDispatcher(tg, st, bot.DispatcherMessages{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutdown signal received")
		cancel()
	}()

	// Start command dispatcher in background
	go func() {
		if err := dispatcher.Start(ctx); err != nil {
			log.Printf("dispatcher error: %v", err)
		}
	}()

	// Create and run the bot
	runner := bot.New(bot.Config{
		Name:         "minimal-rss",
		Source:       NewRSSSource(feedURL),
		Formatter:    &RSSFormatter{},
		Notifier:     tg,
		Store:        st,
		PollInterval: 15 * time.Minute,
		InitialDelay: 10 * time.Second,
	})

	if err := runner.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("runner error: %v", err)
	}
}
