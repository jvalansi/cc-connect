package discord

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// backfillServer serves the two REST endpoints backfillMissed needs: the
// channel lookup (for guild_id, which the messages endpoint omits) and the
// message history. msgs must be newest-first, as Discord returns it.
func backfillServer(t *testing.T, channelID, guildID string, msgs []map[string]any) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var afterSeen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/channels/"+channelID+"/messages"):
			mu.Lock()
			afterSeen = append(afterSeen, r.URL.Query().Get("after"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(msgs)
		case strings.HasSuffix(r.URL.Path, "/channels/"+channelID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": channelID, "guild_id": guildID, "name": "general", "type": 0,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "not found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func msgJSON(id, content, authorID string, mentions ...string) map[string]any {
	ms := []map[string]any{}
	for _, id := range mentions {
		ms = append(ms, map[string]any{"id": id, "username": "bot"})
	}
	return map[string]any{
		"id":         id,
		"content":    content,
		"channel_id": "chan-1",
		"timestamp":  time.Now().Format(time.RFC3339Nano),
		"author":     map[string]any{"id": authorID, "username": "human", "bot": false},
		"mentions":   ms,
	}
}

func newBackfillPlatform(t *testing.T, opts map[string]any, srv *httptest.Server) (*Platform, *[]string) {
	t.Helper()
	opts["token"] = "discord-token"
	pAny, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	p := pAny.(*Platform)
	p.session = newTestDiscordSession(t, srv)
	p.botID = "bot-1"

	var got []string
	p.handler = func(_ core.Platform, msg *core.Message) {
		got = append(got, msg.Content)
	}
	return p, &got
}

func TestBackfillMissed_ReplaysInArrivalOrderAndAdvancesAnchor(t *testing.T) {
	// Newest-first, as Discord returns it.
	srv := backfillServer(t, "chan-1", "", []map[string]any{
		msgJSON("30", "third", "user-1"),
		msgJSON("20", "second", "user-1"),
		msgJSON("10", "first", "user-1"),
	})
	p, got := newBackfillPlatform(t, map[string]any{}, srv)
	p.rememberLastSeen("chan-1", "5")

	p.backfillMissed()

	want := []string{"first", "second", "third"}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Fatalf("replayed = %v, want %v (arrival order)", *got, want)
	}
	if anchor := p.lastSeenSnapshot()["chan-1"]; anchor != "30" {
		t.Fatalf("anchor = %q, want 30 (newest replayed message)", anchor)
	}
}

func TestBackfillMissed_SkipsMessagesAlreadyDeliveredByGateway(t *testing.T) {
	srv := backfillServer(t, "chan-1", "", []map[string]any{
		msgJSON("20", "second", "user-1"),
		msgJSON("10", "first", "user-1"),
	})
	p, got := newBackfillPlatform(t, map[string]any{}, srv)
	p.rememberLastSeen("chan-1", "5")
	// The gateway already delivered "10" before the connection dropped.
	rememberDedupID(&p.seenMsgs, "10")

	p.backfillMissed()

	if fmt.Sprint(*got) != fmt.Sprint([]string{"second"}) {
		t.Fatalf("replayed = %v, want [second]: dedup must drop the overlap", *got)
	}
}

// A guild message must still require a bot mention when replayed. The REST
// history endpoint omits guild_id, and an empty GuildID reads as a DM, so
// without the channel lookup in backfillMissed this answers unprompted.
func TestBackfillMissed_KeepsMentionGatingForGuildMessages(t *testing.T) {
	srv := backfillServer(t, "chan-1", "guild-1", []map[string]any{
		msgJSON("20", "<@bot-1> answer me", "user-1", "bot-1"),
		msgJSON("10", "unrelated chatter", "user-1"),
	})
	p, got := newBackfillPlatform(t, map[string]any{}, srv)
	p.rememberLastSeen("chan-1", "5")

	p.backfillMissed()

	if len(*got) != 1 {
		t.Fatalf("replayed = %v, want only the mention", *got)
	}
	if strings.Contains((*got)[0], "unrelated") {
		t.Fatalf("replayed %q, want the mentioning message", (*got)[0])
	}
}

func TestBackfillMissed_SkipsChannelWhenGuildCannotBeResolved(t *testing.T) {
	// Server 404s the channel lookup, so guild membership is unknown and the
	// mention gating cannot be applied safely.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	t.Cleanup(srv.Close)

	p, got := newBackfillPlatform(t, map[string]any{}, srv)
	p.rememberLastSeen("chan-1", "5")

	p.backfillMissed()

	if len(*got) != 0 {
		t.Fatalf("replayed = %v, want nothing when the channel cannot be resolved", *got)
	}
}

func TestRememberLastSeen_ComparesSnowflakesNumerically(t *testing.T) {
	p := &Platform{lastSeenMsgs: map[string]string{}}

	// "9" > "100" as strings, but 9 < 100 as snowflakes.
	p.rememberLastSeen("chan-1", "100")
	p.rememberLastSeen("chan-1", "9")
	if got := p.lastSeenSnapshot()["chan-1"]; got != "100" {
		t.Fatalf("anchor = %q, want 100: anchor must never move backwards", got)
	}

	p.rememberLastSeen("chan-1", "101")
	if got := p.lastSeenSnapshot()["chan-1"]; got != "101" {
		t.Fatalf("anchor = %q, want 101", got)
	}
}

// The watchdog measures from the start of an outage, so a repeated loss must
// not push the deadline back and let a deaf connection live forever.
func TestMarkGatewayUnconfirmed_KeepsOutageStartTime(t *testing.T) {
	p := &Platform{}
	p.markGatewayConfirmed("ready")

	p.markGatewayUnconfirmed("disconnected")
	p.gwMu.Lock()
	first := p.gwUnconfirmedAt
	p.gwMu.Unlock()
	if first.IsZero() {
		t.Fatal("gwUnconfirmedAt not set on the confirmed -> unconfirmed transition")
	}

	time.Sleep(2 * time.Millisecond)
	p.markGatewayUnconfirmed("disconnected again")
	p.gwMu.Lock()
	second, confirmed := p.gwUnconfirmedAt, p.gwConfirmed
	p.gwMu.Unlock()

	if confirmed {
		t.Fatal("gwConfirmed = true after a loss")
	}
	if !second.Equal(first) {
		t.Fatalf("gwUnconfirmedAt moved from %v to %v; the outage clock must not reset", first, second)
	}

	p.markGatewayConfirmed("resumed")
	p.gwMu.Lock()
	cleared, ok := p.gwUnconfirmedAt.IsZero(), p.gwConfirmed
	p.gwMu.Unlock()
	if !ok || !cleared {
		t.Fatalf("after reconfirmation: confirmed = %v, outage clock cleared = %v, want both true", ok, cleared)
	}
}

// A connection that is opened but never confirmed -- the failure mode that
// went undetected for five minutes -- must still start the outage clock, or
// the watchdog has nothing to measure against and never fires.
func TestArmGatewayClock_StartsOutageClockForUnconfirmedConnection(t *testing.T) {
	p := &Platform{}
	p.armGatewayClock()

	p.gwMu.Lock()
	armed := p.gwUnconfirmedAt
	p.gwMu.Unlock()
	if armed.IsZero() {
		t.Fatal("outage clock not armed for a connection that never confirmed")
	}

	// Re-arming must not push the deadline back.
	time.Sleep(2 * time.Millisecond)
	p.armGatewayClock()
	p.gwMu.Lock()
	again := p.gwUnconfirmedAt
	p.gwMu.Unlock()
	if !again.Equal(armed) {
		t.Fatalf("outage clock moved from %v to %v on re-arm", armed, again)
	}

	// A confirmed connection must not be dragged back to unconfirmed.
	p.markGatewayConfirmed("ready")
	p.armGatewayClock()
	p.gwMu.Lock()
	confirmed, clock := p.gwConfirmed, p.gwUnconfirmedAt
	p.gwMu.Unlock()
	if !confirmed || !clock.IsZero() {
		t.Fatalf("after confirmation: confirmed = %v, clock = %v, want true and zero", confirmed, clock)
	}
}
