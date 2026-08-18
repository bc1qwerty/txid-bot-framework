package notify

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// capture records what the fake Bot API server received.
type capture struct {
	method      string // sendPhoto / sendMessage
	contentType string
	fields      map[string]string
	fileName    string
	fileBytes   []byte
}

// newStubTelegram returns a Telegram notifier pointed at a local server
// that records requests instead of reaching api.telegram.org, plus the
// slice the calls land in. This keeps the photo-upload path testable
// without posting to a real channel.
func newStubTelegram(t *testing.T) (*Telegram, *[]capture) {
	t.Helper()
	var calls []capture

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if method == "getMe" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"stub"}}`)
			return
		}

		c := capture{method: method, contentType: r.Header.Get("Content-Type"), fields: map[string]string{}}
		mediaType, params, _ := mime.ParseMediaType(c.contentType)
		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				body, _ := io.ReadAll(part)
				if part.FileName() != "" {
					c.fileName = part.FileName()
					c.fileBytes = body
					continue
				}
				c.fields[part.FormName()] = string(body)
			}
		} else {
			_ = r.ParseForm()
			for k, v := range r.PostForm {
				c.fields[k] = v[0]
			}
		}
		calls = append(calls, c)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	t.Cleanup(srv.Close)

	api, err := tgbotapi.NewBotAPIWithAPIEndpoint("stub-token", srv.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("stub bot api: %v", err)
	}
	return &Telegram{api: api}, &calls
}

// TestSendImageDataUploadsPhoto locks in the 중대재해 사이렌 path: a source
// that holds the image in memory (no public URL) must reach the channel
// as an actual photo upload with the text as its caption, not as a bare
// link the reader has to click through.
func TestSendImageDataUploadsPhoto(t *testing.T) {
	tg, calls := newStubTelegram(t)

	png := []byte("\x89PNG\r\n\x1a\n fake image bytes")
	err := tg.Send(context.Background(), "@safety_alarm_korea", core.Message{
		Text:      "📢 <b>[중대재해 사이렌]</b> 새 공지사항",
		PlainText: "[중대재해 사이렌] 새 공지사항",
		ParseMode: "HTML",
		ImageData: png,
		ImageName: "kosha_accident-837.png",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("want 1 API call, got %d", len(*calls))
	}
	c := (*calls)[0]
	if c.method != "sendPhoto" {
		t.Errorf("method = %q, want sendPhoto", c.method)
	}
	if !strings.HasPrefix(c.contentType, "multipart/form-data") {
		t.Errorf("content type = %q, want multipart upload", c.contentType)
	}
	if c.fileName != "kosha_accident-837.png" {
		t.Errorf("file name = %q, want kosha_accident-837.png", c.fileName)
	}
	if string(c.fileBytes) != string(png) {
		t.Errorf("uploaded bytes differ from the source image")
	}
	if got := c.fields["caption"]; !strings.Contains(got, "중대재해 사이렌") {
		t.Errorf("caption = %q, want the formatted text", got)
	}
	if got := c.fields["parse_mode"]; got != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", got)
	}
	if got := c.fields["chat_id"]; got != "@safety_alarm_korea" {
		t.Errorf("chat_id = %q, want the @username channel", got)
	}
}

// TestSendImageDataBeatsImageURL — a source that supplies both should
// upload its own bytes rather than ask Telegram to fetch a URL, since
// the bytes are what it actually verified.
func TestSendImageDataBeatsImageURL(t *testing.T) {
	tg, calls := newStubTelegram(t)

	if err := tg.Send(context.Background(), "-1003638685140", core.Message{
		Text:      "hello",
		ImageURL:  "https://example.com/x.jpg",
		ImageData: []byte("bytes"),
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	c := (*calls)[0]
	if c.method != "sendPhoto" || c.fileName == "" {
		t.Fatalf("want a multipart sendPhoto, got method=%q file=%q", c.method, c.fileName)
	}
	if c.fileName != "image.jpg" {
		t.Errorf("file name = %q, want the image.jpg default", c.fileName)
	}
	if got := c.fields["chat_id"]; got != "-1003638685140" {
		t.Errorf("chat_id = %q", got)
	}
}

// TestSendWithoutImageStaysText guards the sources that have no image:
// they must keep going out as plain messages.
func TestSendWithoutImageStaysText(t *testing.T) {
	tg, calls := newStubTelegram(t)

	if err := tg.Send(context.Background(), "@safety_alarm_korea", core.Message{
		Text:      "no image here",
		ParseMode: "HTML",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].method != "sendMessage" {
		t.Fatalf("want a single sendMessage, got %+v", *calls)
	}
}

// TestSendOverlongCaptionAlsoSendsText — Telegram caps captions at 1024
// chars, so an over-long body would either be rejected or truncated.
// The photo goes out uncaptioned and the full text follows as its own
// message; losing either half would be worse than two messages.
func TestSendOverlongCaptionAlsoSendsText(t *testing.T) {
	tg, calls := newStubTelegram(t)

	long := strings.Repeat("가", 1500)
	if err := tg.Send(context.Background(), "@safety_alarm_korea", core.Message{
		Text:      long,
		ImageData: []byte("bytes"),
		ImageName: "a.png",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("want sendPhoto + sendMessage, got %d calls", len(*calls))
	}
	if (*calls)[0].method != "sendPhoto" || (*calls)[0].fields["caption"] != "" {
		t.Errorf("photo should carry no caption, got %q", (*calls)[0].fields["caption"])
	}
	if (*calls)[1].method != "sendMessage" || (*calls)[1].fields["text"] != long {
		t.Errorf("full text should follow as its own message")
	}
}

// TestSendFileDataUploadsDocument locks in the 자료실 path: a KOSHA OPS
// sheet is a PDF, and the reader needs the file itself, not a preview.
// It must go out as sendDocument (photos get re-encoded, which shreds
// small type) with the original filename preserved.
func TestSendFileDataUploadsDocument(t *testing.T) {
	tg, calls := newStubTelegram(t)

	pdf := []byte("%PDF-1.4 stub")
	err := tg.Send(context.Background(), "@safety_alarm_korea", core.Message{
		Text:      "본문",
		ParseMode: "HTML",
		FileData:  pdf,
		FileName:  "[2026-교육총괄실-615]키메시지.pdf",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(*calls), *calls)
	}
	c := (*calls)[0]
	if c.method != "sendDocument" {
		t.Errorf("method = %q, want sendDocument", c.method)
	}
	// The channel form of a document has no NewDocumentToChannel helper in
	// v5, so the username is set on BaseChat by hand — assert it survives.
	if got := c.fields["chat_id"]; got != "@safety_alarm_korea" {
		t.Errorf("chat_id = %q, want @safety_alarm_korea", got)
	}
	if c.fileName != "[2026-교육총괄실-615]키메시지.pdf" {
		t.Errorf("filename = %q, original name not preserved", c.fileName)
	}
	if string(c.fileBytes) != string(pdf) {
		t.Errorf("uploaded bytes differ from source")
	}
	if c.fields["caption"] != "본문" {
		t.Errorf("caption = %q, want 본문", c.fields["caption"])
	}
}

// TestSendFileDataBeatsImageData: when a source supplies both, the document
// is the material and the image is only a thumbnail. Sending both would
// post every archive item twice.
func TestSendFileDataBeatsImageData(t *testing.T) {
	tg, calls := newStubTelegram(t)

	err := tg.Send(context.Background(), "@ch", core.Message{
		Text:      "본문",
		FileData:  []byte("%PDF-1.4 stub"),
		FileName:  "doc.pdf",
		ImageData: []byte("\x89PNG\r\n\x1a\n thumb"),
		ImageName: "thumb.png",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 call (document only), got %d", len(*calls))
	}
	if (*calls)[0].method != "sendDocument" {
		t.Errorf("method = %q, want sendDocument", (*calls)[0].method)
	}
}

// TestSendFileDataOverlongCaptionAlsoSendsText mirrors the photo rule:
// Telegram rejects the whole send when a caption overflows, so the file
// goes out bare and the body follows as its own message. The thumbnail
// must still not be sent — that was a real bug in the first draft.
func TestSendFileDataOverlongCaptionAlsoSendsText(t *testing.T) {
	tg, calls := newStubTelegram(t)

	long := strings.Repeat("가", telegramCaptionLimit+1)
	err := tg.Send(context.Background(), "@ch", core.Message{
		Text:      long,
		FileData:  []byte("%PDF-1.4 stub"),
		FileName:  "doc.pdf",
		ImageData: []byte("\x89PNG\r\n\x1a\n thumb"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected document + text, got %d calls", len(*calls))
	}
	if (*calls)[0].method != "sendDocument" || (*calls)[1].method != "sendMessage" {
		t.Errorf("calls = %q/%q, want sendDocument/sendMessage",
			(*calls)[0].method, (*calls)[1].method)
	}
	if (*calls)[0].fields["caption"] != "" {
		t.Errorf("overlong caption should have been dropped, got %q", (*calls)[0].fields["caption"])
	}
}
