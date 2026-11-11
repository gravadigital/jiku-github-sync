package worker

import (
	"context"
	"encoding/json"

	jikugo "github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/events"
)

// JikuEvents reads requirement events from Jiku's JIKU_EVENTS stream through a durable consumer,
// so events published while the connector was down are delivered when it comes back (within the
// stream's retention). Delivery is at-least-once; the engine is idempotent, so duplicates are
// harmless and need no dedup store.
type JikuEvents struct {
	Client  *jikugo.Client
	Durable string
}

// Run implements Events.
func (j JikuEvents) Run(ctx context.Context, handle func(Event)) error {
	cons, err := events.New(j.Client)
	if err != nil {
		return err
	}
	defer cons.Close()
	return cons.Subscribe(ctx, events.Options{Filter: "requirement.>", Durable: j.Durable}, func(ev events.Event) error {
		if ev.Entity.Type != events.EntityRequirement {
			return nil
		}
		out := Event{ID: ev.EventID, Type: ev.Type, ReqID: ev.Entity.ID, ProjectID: ev.Entity.ProjectID, ActorID: ev.Actor.ID}
		if c, ok := ev.Change("visibilityLevel"); ok {
			var to string
			if json.Unmarshal(c.To, &to) == nil && to == "public" {
				out.BecamePublic = true
			}
		}
		handle(out)
		// Never return an error: it would stop the subscription. A failed sync is retried by
		// the reconciliation.
		return nil
	})
}
