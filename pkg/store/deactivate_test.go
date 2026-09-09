package store

import "testing"

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:", "test-bot")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// 한 사람이 조건별로 여러 슬롯을 갖는 봇(nara-bot: `5385429383:1`·`:2`)에서는
// chat_id 하나만 끄면 나머지가 계속 운다. recipient 로 전부 끊어야 한다.
func TestDeactivateRecipientCoversEverySlot(t *testing.T) {
	st := openTestStore(t)
	for _, s := range []Subscription{
		{ID: "5385429383:1", Recipient: "5385429383"},
		{ID: "5385429383:2", Recipient: "5385429383"},
		{ID: "999:1", Recipient: "999"},
	} {
		if err := st.SubscribeRich(s); err != nil {
			t.Fatalf("subscribe %s: %v", s.ID, err)
		}
	}

	n, err := st.DeactivateRecipient("5385429383")
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if n != 2 {
		t.Fatalf("끈 개수 = %d, want 2", n)
	}
	subs, _ := st.ActiveSubscriptions()
	if len(subs) != 1 || subs[0].ID != "999:1" {
		t.Fatalf("남은 구독이 틀렸다: %v", subs)
	}
}

// ⚠옛 행은 recipient 가 빈 문자열이고 그때는 chat_id 가 곧 수신자다
// (ActiveSubscriptions 의 폴백). 여기서 그 폴백을 빠뜨리면 옛 행은 영영 안 꺼진다.
func TestDeactivateRecipientHandlesLegacyRows(t *testing.T) {
	st := openTestStore(t)
	if err := st.Subscribe("100"); err != nil { // ID = Recipient = "100"
		t.Fatalf("subscribe: %v", err)
	}
	// recipient 컬럼이 비어 있던 옛 행을 그대로 재현한다.
	if _, err := st.db.Exec(
		`UPDATE bot_subscribers SET recipient = '' WHERE bot_key = ? AND chat_id = ?`,
		st.botKey, "100"); err != nil {
		t.Fatalf("legacy setup: %v", err)
	}

	n, err := st.DeactivateRecipient("100")
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if n != 1 {
		t.Fatalf("옛 행이 꺼지지 않았다: n=%d", n)
	}
	subs, _ := st.ActiveSubscriptions()
	if len(subs) != 0 {
		t.Fatalf("아직 활성: %v", subs)
	}
}

// 이미 꺼진 행을 다시 세지 않는다 — 알림 문구의 "구독 N건" 이 부풀면
// 사람이 상황을 잘못 읽는다.
func TestDeactivateRecipientIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if n, _ := st.DeactivateRecipient("100"); n != 1 {
		t.Fatalf("1회차 n=%d", n)
	}
	if n, _ := st.DeactivateRecipient("100"); n != 0 {
		t.Fatalf("2회차에도 셌다: n=%d", n)
	}
}
