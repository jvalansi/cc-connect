package discord

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	// gatewaySessionConfirmTimeout is how long a gateway connection may go
	// without Discord confirming a session (READY/RESUMED) before the
	// platform forces a reconnect.
	//
	// discordgo needs this backstop: Open() accepts whatever packet Discord
	// sends first, only logs a warning when it is not READY/RESUMED, and
	// returns nil regardless (wsapi.go). Its heartbeat watchdog then checks
	// only for heartbeat ACKs, which keep flowing on a socket that Discord
	// has stopped sending events on. Without this timeout such a connection
	// stays "up" and silently deaf forever.
	gatewaySessionConfirmTimeout = 45 * time.Second
	gatewayWatchdogInterval      = 15 * time.Second
	gatewayRecoverMaxBackoff     = 5 * time.Minute

	// backfillLimit is Discord's per-request maximum for
	// GET /channels/{id}/messages.
	backfillLimit = 100
)

// markGatewayConfirmed records that Discord confirmed a session on the current
// connection. Every confirmation after the first one means we just recovered
// from a gap, so it triggers a backfill of whatever arrived during it.
func (p *Platform) markGatewayConfirmed(via string) {
	p.gwMu.Lock()
	reconnected := p.gwEverConfirmed
	p.gwEverConfirmed = true
	p.gwConfirmed = true
	p.gwUnconfirmedAt = time.Time{}
	p.gwMu.Unlock()

	if !reconnected {
		return
	}
	slog.Info("discord: gateway session reconfirmed", "via", via)
	go p.backfillMissed()
}

// markGatewayUnconfirmed records that the current connection has no confirmed
// session. The timestamp is only advanced on the transition, so the watchdog
// measures from the start of an outage rather than from the last event.
func (p *Platform) markGatewayUnconfirmed(reason string) {
	p.gwMu.Lock()
	stopping := p.gwStopping
	if p.gwConfirmed || p.gwUnconfirmedAt.IsZero() {
		p.gwUnconfirmedAt = time.Now()
	}
	p.gwConfirmed = false
	p.gwMu.Unlock()

	if !stopping {
		slog.Warn("discord: gateway session lost", "reason", reason)
	}
}

// armGatewayClock starts the outage clock for a freshly opened connection that
// Discord has not confirmed yet. Without this, a connection that never
// confirms at all -- as opposed to one that confirms and later goes deaf --
// would leave gwUnconfirmedAt zero and the watchdog would never fire.
func (p *Platform) armGatewayClock() {
	p.gwMu.Lock()
	defer p.gwMu.Unlock()
	if !p.gwConfirmed && p.gwUnconfirmedAt.IsZero() {
		p.gwUnconfirmedAt = time.Now()
	}
}

// watchGateway forces a reconnect when the gateway stays unconfirmed past
// gatewaySessionConfirmTimeout, retrying with backoff while it keeps failing.
func (p *Platform) watchGateway() {
	ticker := time.NewTicker(gatewayWatchdogInterval)
	defer ticker.Stop()

	backoff := gatewayWatchdogInterval
	var lastAttempt time.Time

	for {
		select {
		case <-p.gwStopCh:
			return
		case <-ticker.C:
		}

		p.gwMu.Lock()
		confirmed, since, stopping := p.gwConfirmed, p.gwUnconfirmedAt, p.gwStopping
		p.gwMu.Unlock()

		if stopping {
			return
		}
		if confirmed {
			backoff = gatewayWatchdogInterval
			lastAttempt = time.Time{}
			continue
		}
		if since.IsZero() || time.Since(since) < gatewaySessionConfirmTimeout {
			continue
		}
		if !lastAttempt.IsZero() && time.Since(lastAttempt) < backoff {
			continue
		}

		lastAttempt = time.Now()
		slog.Warn("discord: gateway unconfirmed, forcing reconnect",
			"unconfirmed_for", time.Since(since).Truncate(time.Second))

		if err := p.recoverGateway(); err != nil {
			slog.Error("discord: forced gateway reconnect failed", "error", err, "retry_in", backoff)
			if backoff < gatewayRecoverMaxBackoff {
				backoff *= 2
				if backoff > gatewayRecoverMaxBackoff {
					backoff = gatewayRecoverMaxBackoff
				}
			}
			continue
		}
		backoff = gatewayWatchdogInterval
	}
}

// recoverGateway closes and reopens the gateway connection.
//
// Closing with a normal close code invalidates the session on Discord's side,
// so the RESUME that discordgo sends on the next Open() is answered with Op 9
// and it falls back to a full IDENTIFY -- rather than resuming into the same
// dead session. The discordgo session object is reused rather than replaced:
// every send path on Platform reads p.session without synchronization, so
// swapping the pointer here would be a data race.
func (p *Platform) recoverGateway() error {
	session := p.session
	if session == nil {
		return fmt.Errorf("discord: no gateway session")
	}
	if err := session.Close(); err != nil {
		// Already-closed sockets are expected here; Open() is what matters.
		slog.Debug("discord: closing gateway before reconnect", "error", err)
	}
	if err := session.Open(); err != nil {
		return fmt.Errorf("discord: reopen gateway: %w", err)
	}
	p.armGatewayClock()
	return nil
}

// rememberLastSeen advances the backfill anchor for a channel.
func (p *Platform) rememberLastSeen(channelID, messageID string) {
	if channelID == "" || messageID == "" {
		return
	}
	id, err := strconv.ParseUint(messageID, 10, 64)
	if err != nil {
		return
	}

	p.lastSeenMu.Lock()
	defer p.lastSeenMu.Unlock()
	if prev, ok := p.lastSeenMsgs[channelID]; ok {
		// Snowflakes are monotonic but variable-length, so compare numerically.
		if prevID, err := strconv.ParseUint(prev, 10, 64); err == nil && prevID >= id {
			return
		}
	}
	p.lastSeenMsgs[channelID] = messageID
}

// lastSeenSnapshot copies the backfill anchors so the replay below does not
// hold the lock while making REST calls or dispatching messages.
func (p *Platform) lastSeenSnapshot() map[string]string {
	p.lastSeenMu.Lock()
	defer p.lastSeenMu.Unlock()
	out := make(map[string]string, len(p.lastSeenMsgs))
	for k, v := range p.lastSeenMsgs {
		out[k] = v
	}
	return out
}

// backfillMissed replays messages that arrived while the gateway was down.
//
// Discord does not redeliver events after a session is invalidated, so a
// reconnect on its own silently drops everything sent during the outage. Each
// replayed message goes back through handleMessageCreate, so the existing
// message-ID dedup makes overlap with the gateway harmless.
func (p *Platform) backfillMissed() {
	session := p.session
	if session == nil {
		return
	}

	for channelID, afterID := range p.lastSeenSnapshot() {
		// The REST endpoint omits guild_id, and handleMessageCreate reads an
		// empty GuildID as "DM" -- which would skip the mention gating and
		// answer guild chatter unprompted. Resolve it up front and skip the
		// channel entirely if that fails, rather than guessing.
		ch, err := session.Channel(channelID)
		if err != nil || ch == nil {
			slog.Warn("discord: backfill skipped, cannot resolve channel", "channel", channelID, "error", err)
			continue
		}

		msgs, err := session.ChannelMessages(channelID, backfillLimit, "", afterID, "")
		if err != nil {
			slog.Warn("discord: backfill fetch failed", "channel", channelID, "error", err)
			continue
		}
		if len(msgs) == 0 {
			continue
		}
		if len(msgs) == backfillLimit {
			slog.Warn("discord: backfill hit the per-request fetch limit; older missed messages were not replayed",
				"channel", channelID, "limit", backfillLimit)
		}

		slog.Info("discord: replaying missed messages", "channel", channelID, "count", len(msgs), "after", afterID)

		// ChannelMessages returns newest-first; replay in arrival order.
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m == nil {
				continue
			}
			if m.GuildID == "" {
				m.GuildID = ch.GuildID
			}
			p.handleMessageCreate(session, &discordgo.MessageCreate{Message: m})
		}
	}
}
