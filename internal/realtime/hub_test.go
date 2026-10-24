package realtime

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func recv(c *Client) *string {
	select {
	case m := <-c.Messages():
		s := string(m)
		return &s
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

func TestAudienceRules(t *testing.T) {
	h := NewHub(nil)
	soc, other, unitA, unitB := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	resA := NewClient(uuid.New(), soc, &unitA, "resident_owner")
	resB := NewClient(uuid.New(), soc, &unitB, "resident_owner")
	guard := NewClient(uuid.New(), soc, nil, "guard")
	foreign := NewClient(uuid.New(), other, &unitA, "resident_owner") // same unit id, other society
	for _, c := range []*Client{resA, resB, guard, foreign} {
		h.Register(c)
	}
	ctx := context.Background()

	h.Publish(ctx, Event{Type: "t1", SocietyID: soc, Unit: &unitA})
	if recv(resA) == nil || recv(resB) != nil || recv(guard) != nil || recv(foreign) != nil {
		t.Fatal("unit events must reach only that flat in that society")
	}
	h.Publish(ctx, Event{Type: "t2", SocietyID: soc, Guards: true})
	if recv(guard) == nil || recv(resA) != nil || recv(resB) != nil {
		t.Fatal("guard events must reach only guards")
	}
	h.Publish(ctx, Event{Type: "t3", SocietyID: soc, All: true})
	if recv(resA) == nil || recv(resB) == nil || recv(guard) == nil || recv(foreign) != nil {
		t.Fatal("society events must reach everyone in the society and nobody else")
	}
	h.Publish(ctx, Event{Type: "t4", SocietyID: soc, User: &resB.UserID})
	if recv(resB) == nil || recv(resA) != nil {
		t.Fatal("user events must reach only that person")
	}
	h.Publish(ctx, Event{Type: "t5", SocietyID: soc}) // no audience at all
	for _, c := range []*Client{resA, resB, guard} {
		if recv(c) != nil {
			t.Fatal("an event without audience must go nowhere")
		}
	}
}

func TestConnectionCapDropsOldest(t *testing.T) {
	h := NewHub(nil)
	uid, soc := uuid.New(), uuid.New()
	var cs []*Client
	for i := 0; i < 4; i++ {
		c := NewClient(uid, soc, nil, "guard")
		time.Sleep(2 * time.Millisecond)
		h.Register(c)
		cs = append(cs, c)
	}
	select {
	case <-cs[0].Done():
	default:
		t.Fatal("oldest connection should have been dropped")
	}
	if h.Count() != 3 {
		t.Fatalf("clients: %d", h.Count())
	}
}

func TestRevokeClosesTheUsersConnections(t *testing.T) {
	h := NewHub(nil)
	uid, soc := uuid.New(), uuid.New()
	c := NewClient(uid, soc, nil, "guard")
	h.Register(c)
	h.Publish(context.Background(), Event{Type: "session.revoked", SocietyID: soc, User: &uid})
	if recv(c) == nil {
		t.Fatal("the revoke notice must still be delivered")
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("connection must close after revoke")
	}
}

func TestRedisBridgeReachesOtherInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	a, b := NewHub(rdb), NewHub(rdb)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	soc := uuid.New()
	onA, onB := NewClient(uuid.New(), soc, nil, "guard"), NewClient(uuid.New(), soc, nil, "guard")
	a.Register(onA)
	b.Register(onB)
	a.Publish(ctx, Event{Type: "x", SocietyID: soc, Guards: true})
	if recv(onA) == nil {
		t.Fatal("local delivery")
	}
	if recv(onB) == nil {
		t.Fatal("the other instance must receive it")
	}
	if recv(onA) != nil {
		t.Fatal("no duplicate delivery on the origin instance")
	}
}
