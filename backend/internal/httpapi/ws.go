package httpapi

import (
	"net/http"
	"net/url"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// handleBookWS streams paragraph status updates for bookID to the reader
// in real time (see package wshub), replacing the frontend's previous
// reliance on polling GET /api/books/{id}/chapters/{idx} on an interval
// for every loaded chapter to notice when a paragraph goes from pending to
// generating to ready. The connection is one-way in practice - the client
// doesn't need to send anything after connecting, since it already knows
// its own book id from the URL - but the read loop still has to run so a
// client-initiated close is actually detected.
func (s *Server) handleBookWS(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")

	conn, err := websocket.Accept(w, r, wsAcceptOptions(s.AllowOrigin))
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()
	updates := s.Hub.Subscribe(bookID)
	defer s.Hub.Unsubscribe(bookID, updates)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-closed:
			return
		case u := <-updates:
			if err := wsjson.Write(ctx, conn, u); err != nil {
				return
			}
		}
	}
}

// handleJobsWS streams the job queue's live state to the Jobs dashboard,
// replacing its previous reliance on polling GET /api/jobs on an interval.
// Unlike handleBookWS (which only ever pushes incremental updates, relying
// on a separate initial REST fetch for the starting state), this sends a
// full buildJobsSnapshot immediately on connect and again after every
// change (see jobs.Manager.SubscribeChanges) - every message is the
// complete current state, never a partial patch, so the frontend can
// simply replace what it has on each one rather than reconciling a diff.
func (s *Server) handleJobsWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, wsAcceptOptions(s.AllowOrigin))
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()
	if err := wsjson.Write(ctx, conn, s.buildJobsSnapshot()); err != nil {
		return
	}

	changes := s.Jobs.SubscribeChanges()
	defer s.Jobs.UnsubscribeChanges(changes)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-closed:
			return
		case <-changes:
			if err := wsjson.Write(ctx, conn, s.buildJobsSnapshot()); err != nil {
				return
			}
		}
	}
}

// wsAcceptOptions mirrors withCORS's origin policy for WebSocket upgrades:
// restrict to AllowOrigin's host when one is configured, or skip the check
// entirely when it's unset - consistent with withCORS itself not setting
// any CORS headers (and so not restricting anything) in that case. This
// app has no auth and is meant for single-user local/LAN use, so the
// exposure from a permissive origin check is the same as the REST API's.
func wsAcceptOptions(allowOrigin string) *websocket.AcceptOptions {
	if allowOrigin == "" {
		return &websocket.AcceptOptions{InsecureSkipVerify: true}
	}
	if u, err := url.Parse(allowOrigin); err == nil && u.Host != "" {
		return &websocket.AcceptOptions{OriginPatterns: []string{u.Host}}
	}
	return &websocket.AcceptOptions{InsecureSkipVerify: true}
}
