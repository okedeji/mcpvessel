package findings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var natsTracer = otel.Tracer("agentcage/nats")

type MessageHandler func(ctx context.Context, msg Message) error

type Subscription interface {
	Stop()
}

type Bus interface {
	CreateStream(ctx context.Context, assessmentID string) error
	DeleteStream(ctx context.Context, assessmentID string) error
	Publish(ctx context.Context, assessmentID string, msg Message) error
	Subscribe(ctx context.Context, assessmentID string, handler MessageHandler) (Subscription, error)
	Close()
}

type NATSBus struct {
	conn *nats.Conn
	js   jetstream.JetStream
}

func NewNATSBus(url string, opts ...nats.Option) (*NATSBus, error) {
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}
	return &NATSBus{conn: nc, js: js}, nil
}

func streamName(assessmentID string) string {
	return "findings-" + assessmentID
}

func consumerName(assessmentID string) string {
	return "findings-consumer-" + assessmentID
}

func (b *NATSBus) CreateStream(ctx context.Context, assessmentID string) error {
	_, err := b.js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName(assessmentID),
		Subjects:  []string{Subject(assessmentID)},
		Retention: jetstream.WorkQueuePolicy,
		MaxMsgs:   -1,
		MaxBytes:  -1,
	})
	if err != nil {
		return fmt.Errorf("creating stream for assessment %s: %w", assessmentID, err)
	}
	return nil
}

func (b *NATSBus) DeleteStream(ctx context.Context, assessmentID string) error {
	err := b.js.DeleteStream(ctx, streamName(assessmentID))
	if err != nil {
		return fmt.Errorf("deleting stream for assessment %s: %w", assessmentID, err)
	}
	return nil
}

func (b *NATSBus) Publish(ctx context.Context, assessmentID string, msg Message) error {
	ctx, span := natsTracer.Start(ctx, "nats.publish",
		trace.WithAttributes(attribute.String("nats.subject", Subject(assessmentID))),
	)
	defer span.End()

	data, err := json.Marshal(msg)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("marshaling finding message for assessment %s: %w", assessmentID, err)
	}
	_, err = b.js.Publish(ctx, Subject(assessmentID), data)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("publishing finding to assessment %s: %w", assessmentID, err)
	}
	return nil
}

func (b *NATSBus) Subscribe(ctx context.Context, assessmentID string, handler MessageHandler) (Subscription, error) {
	cons, err := b.js.CreateOrUpdateConsumer(ctx, streamName(assessmentID), jetstream.ConsumerConfig{
		Name:          consumerName(assessmentID),
		Durable:       consumerName(assessmentID),
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    3,
		FilterSubject: Subject(assessmentID),
	})
	if err != nil {
		return nil, fmt.Errorf("creating consumer for assessment %s: %w", assessmentID, err)
	}

	// The subscribe ctx is the Temporal activity context, which cancels
	// when the activity returns. Use Background() for the handler so
	// SaveFinding and other downstream calls survive the activity's
	// lifetime. Subscription teardown is managed via Stop().
	cctx, err := cons.Consume(func(m jetstream.Msg) {
		var msg Message
		if err := json.Unmarshal(m.Data(), &msg); err != nil {
			slog.Error("findings bus: unmarshal failed", "assessment_id", assessmentID, "error", err.Error())
			_ = m.Nak()
			return
		}
		if err := handler(context.Background(), msg); err != nil {
			slog.Error("findings bus: handler failed", "assessment_id", assessmentID, "finding_id", msg.Finding.ID, "error", err.Error())
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return nil, fmt.Errorf("starting consume for assessment %s: %w", assessmentID, err)
	}

	return &natsSub{cctx: cctx}, nil
}

func (b *NATSBus) Conn() *nats.Conn {
	return b.conn
}

func (b *NATSBus) Close() {
	b.conn.Close()
}

type natsSub struct {
	cctx jetstream.ConsumeContext
}

func (s *natsSub) Stop() {
	s.cctx.Stop()
}
