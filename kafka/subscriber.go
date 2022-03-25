package kafka

import (
	"context"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/satioO/scheduler/scheduler/cqrs/message"
)

type Subscriber struct {
	config SubscriberConfig
	logger watermill.LoggerAdapter

	closing       chan struct{}
	subscribersWg sync.WaitGroup

	closed bool
}

// NewSubscriber creates a new Kafka Subscriber.
func NewSubscriber(
	config SubscriberConfig,
	logger watermill.LoggerAdapter,
) (*Subscriber, error) {
	config.setDefaults()

	if err := config.Validate(); err != nil {
		return nil, err
	}

	if logger == nil {
		logger = watermill.NopLogger{}
	}

	logger = logger.With(watermill.LogFields{
		"subscriber_uuid": watermill.NewShortUUID(),
	})

	return &Subscriber{
		config: config,
		logger: logger,

		closing: make(chan struct{}),
	}, nil
}

type SubscriberConfig struct {
	// Kafka brokers list.
	Brokers []string

	// Unmarshaler is used to unmarshal messages from Kafka format into Watermill format.
	Unmarshaler Unmarshaler

	// OverwriteSaramaConfig holds additional sarama settings.
	OverwriteSaramaConfig *sarama.Config

	// Kafka consumer group.
	// When empty, all messages from all partitions will be returned.
	ConsumerGroup string

	// How long after Nack message should be redelivered.
	NackResendSleep time.Duration

	// How long about unsuccessful reconnecting next reconnect will occur.
	ReconnectRetrySleep time.Duration

	InitializeTopicDetails *sarama.TopicDetail
}

// NoSleep can be set to SubscriberConfig.NackResendSleep and SubscriberConfig.ReconnectRetrySleep.
const NoSleep time.Duration = -1

func (c *SubscriberConfig) setDefaults() {
	if c.OverwriteSaramaConfig == nil {
		c.OverwriteSaramaConfig = DefaultSaramaSubscriberConfig()
	}
	if c.NackResendSleep == 0 {
		c.NackResendSleep = time.Millisecond * 100
	}
	if c.ReconnectRetrySleep == 0 {
		c.ReconnectRetrySleep = time.Second
	}
}

func (c SubscriberConfig) Validate() error {
	if len(c.Brokers) == 0 {
		return errors.New("missing brokers")
	}
	if c.Unmarshaler == nil {
		return errors.New("missing unmarshaler")
	}

	return nil
}

// DefaultSaramaSubscriberConfig creates default Sarama config used by Watermill.
//
// Custom config can be passed to NewSubscriber and NewPublisher.
//
//		saramaConfig := DefaultSaramaSubscriberConfig()
//		saramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest
//
//		subscriberConfig.OverwriteSaramaConfig = saramaConfig
//
//		subscriber, err := NewSubscriber(subscriberConfig, logger)
//		// ...
//
func DefaultSaramaSubscriberConfig() *sarama.Config {
	config := sarama.NewConfig()
	config.Version = sarama.V1_0_0_0
	config.Consumer.Return.Errors = true
	config.ClientID = "watermill"

	return config
}

// Subscribe subscribers for messages in Kafka.
//
// There are multiple subscribers spawned
func (s *Subscriber) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	if s.closed {
		return nil, errors.New("subscriber closed")
	}

	s.subscribersWg.Add(1)

	logFields := watermill.LogFields{
		"provider":            "kafka",
		"topic":               topic,
		"consumer_group":      s.config.ConsumerGroup,
		"kafka_consumer_uuid": watermill.NewShortUUID(),
	}
	s.logger.Info("Subscribing to Kafka topic", logFields)

	// we don't want to have buffered channel to not consume message from Kafka when consumer is not consuming
	output := make(chan *message.Message, 0)

	consumeClosed, err := s.consumeMessages(ctx, topic, output, logFields)
	if err != nil {
		s.subscribersWg.Done()
		return nil, err
	}

	go func() {
		// blocking, until s.closing is closed
		s.handleReconnects(ctx, topic, output, consumeClosed, logFields)
		close(output)
		s.subscribersWg.Done()
	}()

	return output, nil
}

func (s *Subscriber) handleReconnects(
	ctx context.Context,
	topic string,
	output chan *message.Message,
	consumeClosed chan struct{},
	logFields watermill.LogFields,
) {
	for {
		// nil channel will cause deadlock
		if consumeClosed != nil {
			<-consumeClosed
			s.logger.Info("consumeMessages stopped", logFields)
		} else {
			s.logger.Info("empty consumeClosed", logFields)
		}

		select {
		// it's important to don't exit before consumeClosed,
		// to not trigger s.subscribersWg.Done() before consumer is closed
		case <-s.closing:
			s.logger.Info("Closing subscriber, no reconnect needed", logFields)
			return
		case <-ctx.Done():
			s.logger.Info("Ctx cancelled, no reconnect needed", logFields)
			return
		default:
			s.logger.Info("Not closing, reconnecting", logFields)
		}

		s.logger.Info("Reconnecting consumer", logFields)

		var err error
		consumeClosed, err = s.consumeMessages(ctx, topic, output, logFields)
		if err != nil {
			s.logger.Error("Cannot reconnect messages consumer", err, logFields)

			if s.config.ReconnectRetrySleep != NoSleep {
				time.Sleep(s.config.ReconnectRetrySleep)
			}
			continue
		}
	}
}

func (s *Subscriber) consumeMessages(
	ctx context.Context,
	topic string,
	output chan *message.Message,
	logFields watermill.LogFields,
) (consumeMessagesClosed chan struct{}, err error) {
	s.logger.Info("Starting consuming", logFields)

	// Start with a client
	client, err := sarama.NewClient(s.config.Brokers, s.config.OverwriteSaramaConfig)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create new Sarama client")
	}

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-s.closing:
			s.logger.Info("Closing subscriber, cancelling consumeMessages", logFields)
			cancel()
		case <-ctx.Done():
			// avoid goroutine leak
		}
	}()

	if s.config.ConsumerGroup == "" {
		consumeMessagesClosed, err = s.consumeWithoutConsumerGroups(ctx, client, topic, output, logFields)
	} else {
		consumeMessagesClosed, err = s.consumeGroupMessages(ctx, client, topic, output, logFields)
	}
	if err != nil {
		s.logger.Info(
			"Starting consume failed, cancelling context",
			logFields.Add(watermill.LogFields{"err": err}),
		)
		cancel()
		return nil, err
	}

	go func() {
		<-consumeMessagesClosed
		if err := client.Close(); err != nil {
			s.logger.Error("Cannot close client", err, logFields)
		} else {
			s.logger.Info("Client closed", logFields)
		}
	}()

	return consumeMessagesClosed, nil
}

func (s *Subscriber) consumeGroupMessages(
	ctx context.Context,
	client sarama.Client,
	topic string,
	output chan *message.Message,
	logFields watermill.LogFields,
) (chan struct{}, error) {
	// Start a new consumer group
	group, err := sarama.NewConsumerGroupFromClient(s.config.ConsumerGroup, client)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create consumer group client")
	}

	groupClosed := make(chan struct{})

	handleGroupErrorsCtx, cancelHandleGroupErrors := context.WithCancel(context.Background())
	handleGroupErrorsDone := s.handleGroupErrors(handleGroupErrorsCtx, group, logFields)

	handler := consumerGroupHandler{
		ctx:              ctx,
		messageHandler:   s.createMessagesHandler(output),
		logger:           s.logger,
		closing:          s.closing,
		messageLogFields: logFields,
	}

	go func() {
		defer func() {
			cancelHandleGroupErrors()
			<-handleGroupErrorsDone

			if err := group.Close(); err != nil {
				s.logger.Info("Group close with error", logFields.Add(watermill.LogFields{"err": err.Error()}))
			}

			s.logger.Info("Consuming done", logFields)
			close(groupClosed)
		}()

	ConsumeLoop:
		for {
			select {
			default:
				s.logger.Info("Not closing", logFields)
			case <-s.closing:
				s.logger.Info("Subscriber is closing, stopping group.Consume loop", logFields)
				break ConsumeLoop
			case <-ctx.Done():
				s.logger.Info("Ctx was cancelled, stopping group.Consume loop", logFields)
				break ConsumeLoop
			}

			if err := group.Consume(ctx, []string{topic}, handler); err != nil {
				if err == sarama.ErrUnknown {
					// this is info, because it is often just noise
					s.logger.Info("Received unknown Sarama error", logFields.Add(watermill.LogFields{"err": err.Error()}))
				} else {
					s.logger.Error("Group consume error", err, logFields)
				}

				break ConsumeLoop
			}

			// this is expected behaviour to run Consume again after it exited
			// see: https://github.com/ThreeDotsLabs/watermill/issues/210
			s.logger.Info("Consume stopped without any error, running consume again", logFields)
		}
	}()

	return groupClosed, nil
}

func (s *Subscriber) handleGroupErrors(
	ctx context.Context,
	group sarama.ConsumerGroup,
	logFields watermill.LogFields,
) chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)
		errs := group.Errors()

		for {
			select {
			case err := <-errs:
				if err == nil {
					continue
				}

				s.logger.Error("Sarama internal error", err, logFields)
			case <-ctx.Done():
				return
			}
		}
	}()

	return done
}

func (s *Subscriber) consumeWithoutConsumerGroups(
	ctx context.Context,
	client sarama.Client,
	topic string,
	output chan *message.Message,
	logFields watermill.LogFields,
) (chan struct{}, error) {
	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create client")
	}

	partitions, err := consumer.Partitions(topic)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get partitions")
	}

	partitionConsumersWg := &sync.WaitGroup{}

	for _, partition := range partitions {
		partitionLogFields := logFields.Add(watermill.LogFields{"kafka_partition": partition})

		partitionConsumer, err := consumer.ConsumePartition(topic, partition, s.config.OverwriteSaramaConfig.Consumer.Offsets.Initial)
		if err != nil {
			if err := client.Close(); err != nil && err != sarama.ErrClosedClient {
				s.logger.Error("Cannot close client", err, partitionLogFields)
			}
			return nil, errors.Wrap(err, "failed to start consumer for partition")
		}

		messageHandler := s.createMessagesHandler(output)

		partitionConsumersWg.Add(1)
		go s.consumePartition(ctx, partitionConsumer, messageHandler, partitionConsumersWg, partitionLogFields)
	}

	closed := make(chan struct{})
	go func() {
		partitionConsumersWg.Wait()
		close(closed)
	}()

	return closed, nil
}

func (s *Subscriber) consumePartition(
	ctx context.Context,
	partitionConsumer sarama.PartitionConsumer,
	messageHandler messageHandler,
	partitionConsumersWg *sync.WaitGroup,
	logFields watermill.LogFields,
) {
	defer func() {
		if err := partitionConsumer.Close(); err != nil {
			s.logger.Error("Cannot close partition consumer", err, logFields)
		}
		partitionConsumersWg.Done()
		s.logger.Info("consumePartition stopped", logFields)

	}()

	kafkaMessages := partitionConsumer.Messages()

	for {
		select {
		case kafkaMsg := <-kafkaMessages:
			if kafkaMsg == nil {
				s.logger.Info("kafkaMsg is closed, stopping consumePartition", logFields)
				return
			}
			if err := messageHandler.processMessage(ctx, kafkaMsg, nil, logFields); err != nil {
				return
			}
		case <-s.closing:
			s.logger.Info("Subscriber is closing, stopping consumePartition", logFields)
			return

		case <-ctx.Done():
			s.logger.Info("Ctx was cancelled, stopping consumePartition", logFields)
			return
		}
	}
}

func (s *Subscriber) createMessagesHandler(output chan *message.Message) messageHandler {
	return messageHandler{
		outputChannel:   output,
		unmarshaler:     s.config.Unmarshaler,
		nackResendSleep: s.config.NackResendSleep,
		logger:          s.logger,
		closing:         s.closing,
	}
}

func (s *Subscriber) Close() error {
	if s.closed {
		return nil
	}

	s.closed = true
	close(s.closing)
	s.subscribersWg.Wait()

	s.logger.Info("Kafka subscriber closed", nil)

	return nil
}

type consumerGroupHandler struct {
	ctx              context.Context
	messageHandler   messageHandler
	logger           watermill.LoggerAdapter
	closing          chan struct{}
	messageLogFields watermill.LogFields
}

func (consumerGroupHandler) Setup(_ sarama.ConsumerGroupSession) error { return nil }

func (consumerGroupHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h consumerGroupHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	kafkaMessages := claim.Messages()
	logFields := h.messageLogFields.Copy().Add(watermill.LogFields{
		"kafka_partition":      claim.Partition(),
		"kafka_initial_offset": claim.InitialOffset(),
	})
	h.logger.Info("Consume claimed", logFields)

	for {
		select {
		case kafkaMsg, ok := <-kafkaMessages:
			if !ok {
				h.logger.Info("kafkaMessages is closed, stopping consumerGroupHandler", logFields)
				return nil
			}
			if err := h.messageHandler.processMessage(h.ctx, kafkaMsg, sess, logFields); err != nil {
				return err
			}

		case <-h.closing:
			h.logger.Info("Subscriber is closing, stopping consumerGroupHandler", logFields)
			return nil

		case <-h.ctx.Done():
			h.logger.Info("Ctx was cancelled, stopping consumerGroupHandler", logFields)
			return nil
		}
	}
}

type messageHandler struct {
	outputChannel chan<- *message.Message
	unmarshaler   Unmarshaler

	nackResendSleep time.Duration

	logger  watermill.LoggerAdapter
	closing chan struct{}
}

func (h messageHandler) processMessage(
	ctx context.Context,
	kafkaMsg *sarama.ConsumerMessage,
	sess sarama.ConsumerGroupSession,
	messageLogFields watermill.LogFields,
) error {
	receivedMsgLogFields := messageLogFields.Add(watermill.LogFields{
		"kafka_partition_offset": kafkaMsg.Offset,
		"kafka_partition":        kafkaMsg.Partition,
	})

	h.logger.Info("Received message from Kafka", receivedMsgLogFields)

	ctx = setPartitionToCtx(ctx, kafkaMsg.Partition)
	ctx = setPartitionOffsetToCtx(ctx, kafkaMsg.Offset)
	ctx = setMessageTimestampToCtx(ctx, kafkaMsg.Timestamp)

	msg, err := h.unmarshaler.Unmarshal(kafkaMsg)
	if err != nil {
		// resend will make no sense, stopping consumerGroupHandler
		return errors.Wrap(err, "message unmarshal failed")
	}

	ctx, cancelCtx := context.WithCancel(ctx)
	msg.SetContext(ctx)
	defer cancelCtx()

	receivedMsgLogFields = receivedMsgLogFields.Add(watermill.LogFields{
		"message_uuid": msg.UUID,
	})

ResendLoop:
	for {
		select {
		case h.outputChannel <- msg:
			h.logger.Trace("Message sent to consumer", receivedMsgLogFields)
		case <-h.closing:
			h.logger.Trace("Closing, message discarded", receivedMsgLogFields)
			return nil
		case <-ctx.Done():
			h.logger.Trace("Closing, ctx cancelled before sent to consumer", receivedMsgLogFields)
			return nil
		}

		select {
		case <-msg.Acked():
			if sess != nil {
				sess.MarkMessage(kafkaMsg, "")
			}
			h.logger.Trace("Message Acked", receivedMsgLogFields)
			break ResendLoop
		case <-msg.Nacked():
			h.logger.Trace("Message Nacked", receivedMsgLogFields)

			// reset acks, etc.
			msg = msg.Copy()
			if h.nackResendSleep != NoSleep {
				time.Sleep(h.nackResendSleep)
			}

			continue ResendLoop
		case <-h.closing:
			h.logger.Trace("Closing, message discarded before ack", receivedMsgLogFields)
			return nil
		case <-ctx.Done():
			h.logger.Trace("Closing, ctx cancelled before ack", receivedMsgLogFields)
			return nil
		}
	}

	return nil
}

func (s *Subscriber) SubscribeInitialize(topic string) (err error) {
	if s.config.InitializeTopicDetails == nil {
		return errors.New("s.config.InitializeTopicDetails is empty, cannot SubscribeInitialize")
	}

	clusterAdmin, err := sarama.NewClusterAdmin(s.config.Brokers, s.config.OverwriteSaramaConfig)
	if err != nil {
		return errors.Wrap(err, "cannot create cluster admin")
	}
	defer func() {
		if closeErr := clusterAdmin.Close(); closeErr != nil {
			err = multierror.Append(err, closeErr)
		}
	}()

	if err := clusterAdmin.CreateTopic(topic, s.config.InitializeTopicDetails, false); err != nil {
		return errors.Wrap(err, "cannot create topic")
	}

	s.logger.Info("Created Kafka topic", watermill.LogFields{"topic": topic})

	return nil
}

type PartitionOffset map[int32]int64

func (s *Subscriber) PartitionOffset(topic string) (PartitionOffset, error) {
	client, err := sarama.NewClient(s.config.Brokers, s.config.OverwriteSaramaConfig)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create new Sarama client")
	}

	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			err = multierror.Append(err, closeErr)
		}
	}()

	partitions, err := client.Partitions(topic)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get topic partitions")
	}

	partitionOffset := make(PartitionOffset, len(partitions))
	for _, partition := range partitions {
		offset, err := client.GetOffset(topic, partition, sarama.OffsetNewest)
		if err != nil {
			return nil, err
		}

		partitionOffset[partition] = offset
	}

	return partitionOffset, nil
}
