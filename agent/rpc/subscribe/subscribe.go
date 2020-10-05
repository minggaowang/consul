package subscribe

import (
	"errors"
	"fmt"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/consul/state"
	"github.com/hashicorp/consul/agent/consul/stream"
	"github.com/hashicorp/consul/proto/pbservice"
	"github.com/hashicorp/consul/proto/pbsubscribe"
)

// Server implements a StateChangeSubscriptionServer for accepting SubscribeRequests,
// and sending events to the subscription topic.
type Server struct {
	Backend Backend
	Logger  Logger
}

func NewServer(backend Backend, logger Logger) *Server {
	return &Server{Backend: backend, Logger: logger}
}

type Logger interface {
	Trace(msg string, args ...interface{})
	With(args ...interface{}) hclog.Logger
}

var _ pbsubscribe.StateChangeSubscriptionServer = (*Server)(nil)

type Backend interface {
	ResolveToken(token string) (acl.Authorizer, error)
	Forward(dc string, f func(*grpc.ClientConn) error) (handled bool, err error)
	Subscribe(req *stream.SubscribeRequest) (*stream.Subscription, error)
}

func (h *Server) Subscribe(req *pbsubscribe.SubscribeRequest, serverStream pbsubscribe.StateChangeSubscription_SubscribeServer) error {
	logger := h.newLoggerForRequest(req)
	handled, err := h.Backend.Forward(req.Datacenter, forwardToDC(req, serverStream, logger))
	if handled || err != nil {
		return err
	}

	logger.Trace("new subscription")
	defer logger.Trace("subscription closed")

	// Resolve the token and create the ACL filter.
	// TODO(streaming): handle token expiry gracefully...
	authz, err := h.Backend.ResolveToken(req.Token)
	if err != nil {
		return err
	}

	sub, err := h.Backend.Subscribe(toStreamSubscribeRequest(req))
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	ctx := serverStream.Context()

	elog := &eventLogger{logger: logger}
	for {
		event, err := sub.Next(ctx)
		switch {
		case errors.Is(err, stream.ErrSubscriptionClosed):
			logger.Trace("subscription reset by server")
			return status.Error(codes.Aborted, err.Error())
		case err != nil:
			return err
		}

		var ok bool
		event, ok = filterByAuth(authz, event)
		if !ok {
			continue
		}

		elog.Trace(event)
		e := newEventFromStreamEvent(req, event)
		if err := serverStream.Send(e); err != nil {
			return err
		}
	}
}

// TODO: can be replaced by mog conversion
func toStreamSubscribeRequest(req *pbsubscribe.SubscribeRequest) *stream.SubscribeRequest {
	return &stream.SubscribeRequest{
		Topic: req.Topic,
		Key:   req.Key,
		Token: req.Token,
		Index: req.Index,
	}
}

func forwardToDC(
	req *pbsubscribe.SubscribeRequest,
	serverStream pbsubscribe.StateChangeSubscription_SubscribeServer,
	logger Logger,
) func(conn *grpc.ClientConn) error {
	return func(conn *grpc.ClientConn) error {
		logger.Trace("forwarding to another DC")
		defer logger.Trace("forwarded stream closed")

		client := pbsubscribe.NewStateChangeSubscriptionClient(conn)
		streamHandle, err := client.Subscribe(serverStream.Context(), req)
		if err != nil {
			return err
		}

		for {
			event, err := streamHandle.Recv()
			if err != nil {
				return err
			}
			if err := serverStream.Send(event); err != nil {
				return err
			}
		}
	}
}

// filterByAuth to only those Events allowed by the acl token.
func filterByAuth(authz acl.Authorizer, event stream.Event) (stream.Event, bool) {
	// authz will be nil when ACLs are disabled
	if authz == nil {
		return event, true
	}
	fn := func(e stream.Event) bool {
		return enforceACL(authz, e) == acl.Allow
	}
	return event.Filter(fn)
}

func newEventFromStreamEvent(req *pbsubscribe.SubscribeRequest, event stream.Event) *pbsubscribe.Event {
	e := &pbsubscribe.Event{
		Topic: req.Topic,
		Key:   req.Key,
		Index: event.Index,
	}
	switch {
	case event.IsEndOfSnapshot():
		e.Payload = &pbsubscribe.Event_EndOfSnapshot{EndOfSnapshot: true}
		return e
	case event.IsEndOfEmptySnapshot():
		e.Payload = &pbsubscribe.Event_EndOfEmptySnapshot{EndOfEmptySnapshot: true}
		return e
	}
	setPayload(e, event.Payload)
	return e
}

func setPayload(e *pbsubscribe.Event, payload interface{}) {
	switch p := payload.(type) {
	case []stream.Event:
		e.Payload = &pbsubscribe.Event_EventBatch{
			EventBatch: &pbsubscribe.EventBatch{
				Events: batchEventsFromEventSlice(p),
			},
		}
	case state.EventPayloadCheckServiceNode:
		e.Payload = &pbsubscribe.Event_ServiceHealth{
			ServiceHealth: &pbsubscribe.ServiceHealthUpdate{
				Op: p.Op,
				// TODO: this could be cached
				CheckServiceNode: pbservice.NewCheckServiceNodeFromStructs(p.Value),
			},
		}
	default:
		panic(fmt.Sprintf("unexpected payload: %T: %#v", p, p))
	}
}

func batchEventsFromEventSlice(events []stream.Event) []*pbsubscribe.Event {
	result := make([]*pbsubscribe.Event, len(events))
	for i := range events {
		event := events[i]
		result[i] = &pbsubscribe.Event{Key: event.Key, Index: event.Index}
		setPayload(result[i], event.Payload)
	}
	return result
}
