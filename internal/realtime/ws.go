package realtime

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	pingEvery = 25 * time.Second
	readLimit = 60 * time.Second
)

// Handler upgrades GET /ws. Browsers cannot set headers on WebSockets, so the access token
// travels in the `token` query parameter; mobile apps may also use the Authorization header.
type Handler struct {
	hub     *Hub
	tokens  *auth.TokenManager
	pool    *pgxpool.Pool
	origins []string
}

func NewHandler(hub *Hub, tokens *auth.TokenManager, pool *pgxpool.Pool, origins []string) *Handler {
	return &Handler{hub: hub, tokens: tokens, pool: pool, origins: origins}
}

func (h *Handler) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" { // native apps send none
		return true
	}
	for _, a := range h.origins {
		if a == "*" || strings.EqualFold(a, o) {
			return true
		}
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if tok == "" {
		httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "missing token"))
		return
	}
	p, err := h.tokens.ParseAccess(tok)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if p.MustChangePassword {
		httpx.Fail(w, httpx.NewError(http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "set a new password to continue"))
		return
	}
	// the membership must still be active and the society usable
	var unit *uuid.UUID
	var ok bool
	err = h.pool.QueryRow(r.Context(), `
		SELECT m.unit_id, true FROM memberships m JOIN societies s ON s.id=m.society_id
		WHERE m.id=$1 AND m.user_id=$2 AND m.society_id=$3 AND m.status='active' AND s.status <> 'suspended'`,
		p.MembershipID, p.UserID, p.SocietyID).Scan(&unit, &ok)
	if err != nil || !ok {
		httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "membership not available"))
		return
	}
	up := websocket.Upgrader{CheckOrigin: h.originOK, ReadBufferSize: 1024, WriteBufferSize: 4096}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := NewClient(p.UserID, p.SocietyID, unit, p.Role)
	h.hub.Register(c)
	go h.write(conn, c)
	h.read(conn, c)
}

func (h *Handler) read(conn *websocket.Conn, c *Client) {
	defer func() {
		h.hub.Unregister(c)
		conn.Close()
	}()
	conn.SetReadLimit(512)
	_ = conn.SetReadDeadline(time.Now().Add(readLimit))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(readLimit)) })
	for { // clients only send pings/keepalives; content is ignored
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(readLimit))
	}
}

func (h *Handler) write(conn *websocket.Conn, c *Client) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	defer conn.Close()
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`))
	for {
		select {
		case m := <-c.send:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if conn.WriteMessage(websocket.TextMessage, m) != nil {
				return
			}
		case <-t.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if conn.WriteMessage(websocket.PingMessage, nil) != nil {
				return
			}
		case <-c.Done():
			// flush what is queued (e.g. session.revoked), then close
			for {
				select {
				case m := <-c.send:
					_ = conn.WriteMessage(websocket.TextMessage, m)
				default:
					_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
					return
				}
			}
		}
	}
}
