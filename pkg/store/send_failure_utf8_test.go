package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// last_error 는 500 바이트로 자른다. 경계에서 한글이 반 토막 나면 깨진
// UTF-8 이 TEXT 컬럼에 그대로 박힌다.
func TestRecordSendFailureKeepsValidUTF8(t *testing.T) {
	st := openTestStore(t)
	msg := "a" + strings.Repeat("가", 200) // 1+600 바이트, 500 은 글자 중간
	if _, err := st.RecordSendFailure("g2b", "u1", msg); err != nil {
		t.Fatalf("record: %v", err)
	}
	var got string
	if err := st.DB().QueryRow(`SELECT last_error FROM bot_send_failure WHERE item_id='u1'`).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !utf8.ValidString(got) || len(got) > 500 {
		t.Fatalf("깨진 UTF-8 또는 상한 초과: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}
