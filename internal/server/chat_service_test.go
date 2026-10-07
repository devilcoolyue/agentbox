package server

import (
	"context"
	"testing"

	"agentbox/internal/chat"
	"agentbox/internal/store"
)

type panicChatHistory struct{ chatRuntime }

func (r panicChatHistory) Record(entry chat.Entry, kind, retry string) error {
	if entry.Kind == "event" {
		panic("synthetic private panic must not become a successful receipt")
	}
	return r.chatRuntime.Record(entry, kind, retry)
}

func TestChatServicePanicSettlesBeforeUncertainReceiptAndRoomRelease(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	if _, err := f.s.store.Grant(f.u.Name, 1000, "synthetic", "", "admin"); err != nil {
		t.Fatal(err)
	}
	room := f.s.chat.room(f.sess.ID)
	if err := room.begin(); err != nil {
		t.Fatal(err)
	}
	row, fresh, err := f.s.store.AcceptChatRequest(f.u, f.sess.ID, newOperationID(), f.input)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	room.requestID = row.RequestID
	observer := &chatReceiptTurn{room: room, owner: f.u, receipt: row, outcome: store.ChatUncertain, code: "execution_incomplete"}
	f.unblock()
	func() {
		defer room.end()
		service := chat.Service{Runtime: panicChatHistory{chatRuntime{room}}, Usage: f.s.usageService()}
		service.Run(context.Background(), chat.Input{SessionID: f.sess.ID, Text: f.input.Text, Model: f.input.Model}, observer)
	}()
	result, err := f.s.store.ChatRequest(f.u, f.sess.ID, row.RequestID)
	if err != nil || result.State != store.ChatUncertain || result.ErrorCode != "execution_incomplete" {
		t.Fatal(result, err)
	}
	rows := f.s.store.ListUsage(store.UsageFilter{User: f.u.Name})
	quota, _ := f.s.store.GetQuota(f.u.Name)
	if len(rows) != 1 || rows[0].CostMicroUSD != 150 || quota.BalanceMicroUSD != 850 || room.state() != "idle" || f.invocations.Load() != 1 {
		t.Fatal("panic lost settlement or released ownership incorrectly", rows, quota)
	}
}
