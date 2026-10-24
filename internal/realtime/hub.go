// Package realtime delivers server events to connected apps over WebSockets.
// Events are society-scoped; a Redis pub/sub bridge fans them out across API instances.
package realtime

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const channel = "nexora:events"

// Event is one message. Audience fields decide who receives it; at least one must be set.
type Event struct {
	Type      string     `json:"type"`
	SocietyID uuid.UUID  `json:"society_id"`
	Unit      *uuid.UUID `json:"unit_id,omitempty"` // residents living in this flat
	Guards    bool       `json:"guards,omitempty"`  // every guard of the society
	Roles     []string   `json:"roles,omitempty"`   // everyone holding one of these roles (e.g. society_admin)
	All       bool       `json:"all,omitempty"`     // everyone in the society
	User      *uuid.UUID `json:"user_id,omitempty"` // one person (all their devices)
	Data      any        `json:"data,omitempty"`
}

// Wire is what a client receives.
type Wire struct {
	Type string    `json:"type"`
	Data any       `json:"data,omitempty"`
	At   time.Time `json:"at"`
}

// Client is one connected device.
type Client struct {
	UserID    uuid.UUID
	SocietyID uuid.UUID
	UnitID    *uuid.UUID
	Role      string
	send      chan []byte
	closeOnce sync.Once
	done      chan struct{}
	since     time.Time
}

func NewClient(userID, societyID uuid.UUID, unitID *uuid.UUID, role string) *Client {
	return &Client{UserID: userID, SocietyID: societyID, UnitID: unitID, Role: role, send: make(chan []byte, 32), done: make(chan struct{}), since: time.Now()}
}

// Messages is the outgoing queue; the connection writer drains it.
func (c *Client) Messages() <-chan []byte { return c.send }

// Done closes when the hub drops the client.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) close() { c.closeOnce.Do(func() { close(c.done) }) }

func (c *Client) matches(e Event) bool {
	if c.SocietyID != e.SocietyID {
		return false
	}
	switch {
	case e.All:
		return true
	case e.User != nil && *e.User == c.UserID:
		return true
	case e.Guards && c.Role == "guard":
		return true
	case len(e.Roles) > 0 && slices.Contains(e.Roles, c.Role):
		return true
	case e.Unit != nil && c.UnitID != nil && *e.Unit == *c.UnitID && c.Role != "guard":
		return true
	}
	return false
}

type envelope struct {
	Origin string `json:"o"`
	Event  Event  `json:"e"`
}

// Hub keeps the connected clients.
type Hub struct {
	rdb        *redis.Client
	id         string
	maxPerUser int
	mu         sync.RWMutex
	clients    map[*Client]struct{}
}

func NewHub(rdb *redis.Client) *Hub {
	return &Hub{rdb: rdb, id: uuid.NewString(), maxPerUser: 3, clients: map[*Client]struct{}{}}
}

// Register adds a client, dropping the oldest connection of the same user beyond the cap.
func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var mine []*Client
	for o := range h.clients {
		if o.UserID == c.UserID {
			mine = append(mine, o)
		}
	}
	for len(mine) >= h.maxPerUser {
		oldest := 0
		for i, o := range mine {
			if o.since.Before(mine[oldest].since) {
				oldest = i
			}
		}
		delete(h.clients, mine[oldest])
		mine[oldest].close()
		mine = append(mine[:oldest], mine[oldest+1:]...)
	}
	h.clients[c] = struct{}{}
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.close()
}

// Count is the number of connected clients (tests, metrics).
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Publish delivers to local clients and to the other API instances.
func (h *Hub) Publish(ctx context.Context, e Event) {
	h.deliver(e)
	if h.rdb != nil {
		if b, err := json.Marshal(envelope{Origin: h.id, Event: e}); err == nil {
			_ = h.rdb.Publish(ctx, channel, b).Err()
		}
	}
}

func (h *Hub) deliver(e Event) {
	msg, err := json.Marshal(Wire{Type: e.Type, Data: e.Data, At: time.Now().UTC()})
	if err != nil {
		return
	}
	var revoke bool
	if e.Type == "session.revoked" {
		revoke = true
	}
	h.mu.RLock()
	var drop []*Client
	for c := range h.clients {
		if !c.matches(e) {
			continue
		}
		select {
		case c.send <- msg:
		default: // slow client: drop it, it will reconnect and resync
			drop = append(drop, c)
		}
		if revoke {
			drop = append(drop, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range drop {
		h.Unregister(c)
	}
}

// Run listens to other instances until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	if h.rdb == nil {
		return
	}
	sub := h.rdb.Subscribe(ctx, channel)
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-sub.Channel():
			if !ok {
				return
			}
			var env envelope
			if json.Unmarshal([]byte(m.Payload), &env) != nil || env.Origin == h.id {
				continue
			}
			h.deliver(env.Event)
		}
	}
}
