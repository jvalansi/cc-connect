package claudecode

import (
	"bytes"
	"encoding/json"
	"testing"
)

type bufStdin struct{ bytes.Buffer }

func (*bufStdin) Close() error { return nil }

func TestInterruptTurn(t *testing.T) {
	in := &bufStdin{}
	cs := &claudeSession{stdin: in}
	if err := cs.InterruptTurn(); err == nil {
		t.Fatal("want error when the process is not running")
	}
	cs.alive.Store(true)
	if err := cs.InterruptTurn(); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type      string
		RequestID string `json:"request_id"`
		Request   struct{ Subtype string }
	}
	if err := json.Unmarshal(in.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "control_request" || got.RequestID == "" || got.Request.Subtype != "interrupt" {
		t.Fatalf("unexpected request: %s", in.String())
	}
}
