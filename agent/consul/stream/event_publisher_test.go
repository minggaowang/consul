package stream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type intTopic int

func (i intTopic) String() string {
	return fmt.Sprintf("%d", i)
}

var testTopic Topic = intTopic(999)

func TestEventPublisher_PublishChangesAndSubscribe_WithSnapshot(t *testing.T) {
	subscription := &SubscribeRequest{
		Topic: testTopic,
		Key:   "sub-key",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	publisher := NewEventPublisher(newTestSnapshotHandlers(), 0)
	go publisher.Run(ctx)

	sub, err := publisher.Subscribe(subscription)
	require.NoError(t, err)
	eventCh := consumeSubscription(ctx, sub)

	result := nextResult(t, eventCh)
	require.NoError(t, result.Err)
	expected := Event{Payload: "snapshot-event-payload", Key: "sub-key"}
	require.Equal(t, expected, result.Event)

	result = nextResult(t, eventCh)
	require.True(t, result.Event.IsEndOfSnapshot())

	// Now subscriber should block waiting for updates
	assertNoResult(t, eventCh)

	events := []Event{{
		Topic:   testTopic,
		Key:     "sub-key",
		Payload: "the-published-event-payload",
	}}
	publisher.Publish(events)

	// Subscriber should see the published event
	result = nextResult(t, eventCh)
	require.NoError(t, result.Err)
	expected = Event{Payload: "the-published-event-payload", Key: "sub-key", Topic: testTopic}
	require.Equal(t, expected, result.Event)
}

func newTestSnapshotHandlers() SnapshotHandlers {
	return SnapshotHandlers{
		testTopic: func(req SubscribeRequest, buf SnapshotAppender) (uint64, error) {
			if req.Topic != testTopic {
				return 0, fmt.Errorf("unexpected topic: %v", req.Topic)
			}
			buf.Append([]Event{{Payload: "snapshot-event-payload", Key: "sub-key"}})
			return 1, nil
		},
	}
}

func consumeSubscription(ctx context.Context, sub *Subscription) <-chan eventOrErr {
	eventCh := make(chan eventOrErr, 1)
	go func() {
		for {
			es, err := sub.Next(ctx)
			eventCh <- eventOrErr{
				Event: es,
				Err:   err,
			}
			if err != nil {
				return
			}
		}
	}()
	return eventCh
}

type eventOrErr struct {
	Event Event
	Err   error
}

func nextResult(t *testing.T, eventCh <-chan eventOrErr) eventOrErr {
	t.Helper()
	select {
	case next := <-eventCh:
		return next
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("no event after 100ms")
	}
	return eventOrErr{}
}

func assertNoResult(t *testing.T, eventCh <-chan eventOrErr) {
	t.Helper()
	select {
	case next := <-eventCh:
		require.NoError(t, next.Err)
		require.Len(t, next.Event, 1)
		t.Fatalf("received unexpected event: %#v", next.Event.Payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventPublisher_ShutdownClosesSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	handlers := newTestSnapshotHandlers()
	fn := func(req SubscribeRequest, buf SnapshotAppender) (uint64, error) {
		return 0, nil
	}
	handlers[intTopic(22)] = fn
	handlers[intTopic(33)] = fn

	publisher := NewEventPublisher(handlers, time.Second)
	go publisher.Run(ctx)

	sub1, err := publisher.Subscribe(&SubscribeRequest{Topic: intTopic(22)})
	require.NoError(t, err)
	defer sub1.Unsubscribe()

	sub2, err := publisher.Subscribe(&SubscribeRequest{Topic: intTopic(33)})
	require.NoError(t, err)
	defer sub2.Unsubscribe()

	cancel() // Shutdown

	err = consumeSub(context.Background(), sub1)
	require.Equal(t, err, ErrSubscriptionClosed)

	_, err = sub2.Next(context.Background())
	require.Equal(t, err, ErrSubscriptionClosed)
}

func consumeSub(ctx context.Context, sub *Subscription) error {
	for {
		event, err := sub.Next(ctx)
		switch {
		case err != nil:
			return err
		case event.IsEndOfSnapshot():
			continue
		}
	}
}
