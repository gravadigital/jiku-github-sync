package worker

import (
	"context"
	"encoding/json"

	jikugo "github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/events"
)

// JikuEvents reads requirement and task events from Jiku's JIKU_EVENTS stream through a durable consumer,
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
	// Every event: task events are only used by projects with tasks turned on, which a reload of
	// the mapping can change without restarting the subscription.
	return cons.Subscribe(ctx, events.Options{Filter: ">", Durable: j.Durable}, func(ev events.Event) error {
		out := Event{ID: ev.EventID, Type: ev.Type, ProjectID: ev.Entity.ProjectID, ActorID: ev.Actor.ID}
		switch ev.Entity.Type {
		case events.EntityRequirement:
			out.ReqID = ev.Entity.ID
		case events.EntityTask:
			out.TaskID = ev.Entity.ID
			// A task moved to another requirement may now qualify, however old it is.
			if _, ok := ev.Change("requirementId"); ok {
				out.BecamePublic = true
			}
		default:
			return nil
		}
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
