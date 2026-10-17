package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// creator appends dashboard (or webhook) events at a given time.
func (f fx) at(source, typ string, when time.Time, payload any) (Event, error) {
	p, _ := json.Marshal(payload)
	e, _, err := f.s.Append(context.Background(), Event{Owner: f.owner, Source: source, Type: typ, OccurredAt: when, Payload: p})
	return e, err
}

func (f fx) mustAt(source, typ string, when time.Time, payload any) Event {
	f.t.Helper()
	e, err := f.at(source, typ, when, payload)
	if err != nil {
		f.t.Fatalf("%s: %v", typ, err)
	}
	return e
}

func inboxIDs(t *testing.T, s *Store, who string) []string {
	t.Helper()
	ns, err := s.SubscriberInbox(context.Background(), who, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		var text string
		json.Unmarshal(n.Payload, &text)
		out = append(out, text)
	}
	return out
}

// topicStory: a topic, three sends around one subscriber's two periods.
func topicStory(t *testing.T) (fx, Event) {
	s, _ := open(t)
	f := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	topic := f.mustAt("dashboard", TypeTopicCreated, day(1), TopicCreated{Name: "my new stock", Price: Price{Kind: PriceMonthly}})
	send := func(d int, text string) {
		f.mustAt("dashboard", TypeNotificationSent, day(d), NotificationSent{TopicID: topic.ID, Payload: json.RawMessage(`"` + text + `"`)})
	}
	send(2, "before")
	f.mustAt("stripe", TypeTopicSubscribed, day(3), TopicSubscribed{TopicID: topic.ID, Subscriber: " Ann@Example.com "})
	send(4, "during")
	f.mustAt("stripe", TypeTopicUnsubscribed, day(5), TopicUnsubscribed{TopicID: topic.ID, Subscriber: "ann@example.com", Reason: "lapsed"})
	send(6, "lapsed")
	f.mustAt("stripe", TypeTopicSubscribed, day(7), TopicSubscribed{TopicID: topic.ID, Subscriber: "ann@example.com"})
	send(8, "back")
	return f, topic
}

func TestSubscriberInbox(t *testing.T) {
	f, topic := topicStory(t)
	// History from subscription: only what was sent while subscribed.
	if got := inboxIDs(t, f.s, "ANN@example.com"); !reflect.DeepEqual(got, []string{"back", "during"}) {
		t.Fatalf("inbox %v", got)
	}
	if got := inboxIDs(t, f.s, "bob@example.com"); len(got) != 0 {
		t.Fatalf("a stranger's inbox: %v", got)
	}
	// The creator sets history further back: older sends show up too.
	f.mustAt("dashboard", TypeTopicAmended, time.Now(), TopicAmended{TopicID: topic.ID, HistoryStart: ptr(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))})
	if got := inboxIDs(t, f.s, "ann@example.com"); !reflect.DeepEqual(got, []string{"back", "lapsed", "during", "before"}) {
		t.Fatalf("inbox with history start: %v", got)
	}
	tp, err := f.s.Topic(context.Background(), topic.ID)
	if err != nil || tp.Subscribers != 1 || tp.HistoryStart == nil || tp.Price.Currency != "usd" {
		t.Fatalf("topic: %+v %v", tp, err)
	}
	f.mustAt("dashboard", TypeTopicAmended, time.Now(), TopicAmended{TopicID: topic.ID, ClearHistoryStart: true, Cap: ptr(100)})
	if tp, _ = f.s.Topic(context.Background(), topic.ID); tp.HistoryStart != nil || tp.Cap == nil || *tp.Cap != 100 {
		t.Fatalf("after clearing history and capping: %+v", tp)
	}
}

func TestTopicRejects(t *testing.T) {
	f, topic := topicStory(t)
	now := time.Now()
	for name, c := range map[string]struct {
		source, typ string
		payload     any
	}{
		"no name":            {"dashboard", TypeTopicCreated, TopicCreated{Price: Price{Kind: PriceOneTime}}},
		"bad price":          {"dashboard", TypeTopicCreated, TopicCreated{Name: "x", Price: Price{Kind: "weekly"}}},
		"negative":           {"dashboard", TypeTopicCreated, TopicCreated{Name: "x", Price: Price{Kind: PriceOneTime, AmountCents: -1}}},
		"zero cap":           {"dashboard", TypeTopicCreated, TopicCreated{Name: "x", Price: Price{Kind: PriceOneTime}, Cap: ptr(0)}},
		"unknown topic":      {"dashboard", TypeTopicAmended, TopicAmended{TopicID: "nope", Name: "x"}},
		"amend nothing":      {"dashboard", TypeTopicAmended, TopicAmended{TopicID: topic.ID}},
		"set and clear":      {"dashboard", TypeTopicAmended, TopicAmended{TopicID: topic.ID, Cap: ptr(3), ClearCap: true}},
		"no email":           {"stripe", TypeTopicSubscribed, TopicSubscribed{TopicID: topic.ID, Subscriber: "ann"}},
		"twice":              {"stripe", TypeTopicSubscribed, TopicSubscribed{TopicID: topic.ID, Subscriber: "ann@example.com"}},
		"not subscribed":     {"stripe", TypeTopicUnsubscribed, TopicUnsubscribed{TopicID: topic.ID, Subscriber: "bob@example.com"}},
		"empty notification": {"dashboard", TypeNotificationSent, NotificationSent{TopicID: topic.ID}},
		"the agent sends":    {SourceAgent, TypeNotificationSent, NotificationSent{TopicID: topic.ID, Payload: json.RawMessage(`"buy"`)}},
	} {
		_, err := f.at(c.source, c.typ, now, c.payload)
		var inv *InvalidEventError
		if !errors.As(err, &inv) {
			t.Errorf("%s: want InvalidEventError, got %v", name, err)
		}
	}
	// Another owner can't touch the topic.
	other := fx{t: t, s: f.s, owner: addUser(t, f.s, "+2").ID}
	if _, err := other.at("dashboard", TypeNotificationSent, now, NotificationSent{TopicID: topic.ID, Payload: json.RawMessage(`"x"`)}); err == nil {
		t.Fatal("another owner sent on this topic")
	}
}

func TestTopicRebuildEqualsIncremental(t *testing.T) {
	f, _ := topicStory(t)
	before := dump(t, f.s)
	if len(before["topics"]) != 1 || len(before["subscriptions"]) != 2 || len(before["notifications"]) != 4 {
		t.Fatalf("story produced too little to compare: %v", before)
	}
	if err := f.s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := dump(t, f.s); !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuild differs:\nbefore %v\nafter  %v", before, after)
	}
}

func ptr[T any](v T) *T { return &v }
