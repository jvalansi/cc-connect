package core

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleSend_AllowsAttachmentOnly(t *testing.T) {
	engine := NewEngine("test", &stubAgent{}, []Platform{&stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}, "", LangEnglish)
	engine.interactiveStates["session-1"] = &interactiveState{
		platform: &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}},
		replyCtx: "reply-ctx",
	}

	api := &APIServer{engines: map[string]*Engine{"test": engine}}
	reqBody := SendRequest{
		Project:    "test",
		SessionKey: "session-1",
		Images: []ImageAttachment{{
			MimeType: "image/png",
			Data:     []byte("img"),
			FileName: "chart.png",
		}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSend_AgentSession(t *testing.T) {
	engine := NewEngine("test", &stubAgent{}, []Platform{&stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}, "", LangEnglish)
	engine.interactiveStates["session-1"] = &interactiveState{
		platform: &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}},
		replyCtx: "reply-ctx",
	}
	api := &APIServer{engines: map[string]*Engine{"test": engine}}
	send := func(agentSession string) int {
		body, _ := json.Marshal(SendRequest{Project: "test", SessionKey: "session-1", Message: "hi", AgentSessionID: agentSession})
		rec := httptest.NewRecorder()
		api.handleSend(rec, httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body)))
		return rec.Code
	}
	if code := send("../../etc"); code != http.StatusBadRequest {
		t.Fatalf("non-UUID agent session: status = %d, want 400", code)
	}
	// A platform that can't tag posts still gets the message.
	if code := send("11111111-2222-3333-4444-555555555555"); code != http.StatusOK {
		t.Fatalf("untaggable platform: status = %d, want 200", code)
	}
}
