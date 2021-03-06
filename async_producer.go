package sarama

import (
	"fmt"
	"sync"
	"time"

	"github.com/eapache/go-resiliency/breaker"
	"github.com/eapache/queue"
	"sync/atomic"
	"runtime/debug"
	"runtime/pprof"
"os"
)

// AsyncProducer publishes Kafka messages using a non-blocking API. It routes messages
// to the correct broker for the provided topic-partition, refreshing metadata as appropriate,
// and parses responses for errors. You must read from the Errors() channel or the
// producer will deadlock. You must call Close() or AsyncClose() on a producer to avoid
// leaks: it will not be garbage-collected automatically when it passes out of
// scope.
type AsyncProducer interface {

	// AsyncClose triggers a shutdown of the producer, flushing any messages it may
	// have buffered. The shutdown has completed when both the Errors and Successes
	// channels have been closed. When calling AsyncClose, you *must* continue to
	// read from those channels in order to drain the results of any messages in
	// flight.
	AsyncClose()

	// Close shuts down the producer and flushes any messages it may have buffered.
	// You must call this function before a producer object passes out of scope, as
	// it may otherwise leak memory. You must call this before calling Close on the
	// underlying client.
	Close() error

	// Input is the input channel for the user to write messages to that they
	// wish to send.
	Input() chan<- *ProducerMessage

	// Successes is the success output channel back to the user when AckSuccesses is
	// enabled. If Return.Successes is true, you MUST read from this channel or the
	// Producer will deadlock. It is suggested that you send and read messages
	// together in a single select statement.
	Successes() <-chan *ProducerMessage

	// Errors is the error output channel back to the user. You MUST read from this
	// channel or the Producer will deadlock when the channel is full. Alternatively,
	// you can set Producer.Return.Errors in your config to false, which prevents
	// errors to be returned.
	Errors() <-chan *ProducerError
}

type SafeWaitGroup struct {
	wg sync.WaitGroup
	hasPaniced int64
}

func (b *SafeWaitGroup) onPanic(rec interface{}) {
	fmt.Printf("Had to recover from sarama state machine error: %s\n", rec)
	atomic.StoreInt64(&b.hasPaniced, 1)
	Logger.Println(rec)
	debug.PrintStack()
	pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
}

func (b *SafeWaitGroup) Add(delta int) {
	if atomic.LoadInt64(&b.hasPaniced) == 0 {
		defer func() {
			if rec := recover(); rec != nil {
				b.onPanic(rec)
			}
		}()
		b.wg.Add(delta)
	}
}

func (b *SafeWaitGroup) Done() {
	if atomic.LoadInt64(&b.hasPaniced) == 0 {
		defer func() {
			if rec := recover(); rec != nil {
				b.onPanic(rec)
			}
		}()
		b.wg.Done()
	}
}

func (b *SafeWaitGroup) Wait() {
	if atomic.LoadInt64(&b.hasPaniced) == 0 {
		b.wg.Wait()
	}
}

type asyncProducer struct {
	client    Client
	conf      *Config
	ownClient bool

	errors                    chan *ProducerError
	input, successes, retries chan *ProducerMessage
	inFlight                  SafeWaitGroup

	brokers    map[*Broker]chan<- *ProducerMessage
	brokerRefs map[chan<- *ProducerMessage]int
	brokerLock sync.Mutex
}

// NewAsyncProducer creates a new AsyncProducer using the given broker addresses and configuration.
func NewAsyncProducer(addrs []string, conf *Config) (AsyncProducer, error) {
	client, err := NewClient(addrs, conf)
	if err != nil {
		return nil, err
	}

	p, err := NewAsyncProducerFromClient(client)
	if err != nil {
		return nil, err
	}
	p.(*asyncProducer).ownClient = true
	return p, nil
}

// NewAsyncProducerFromClient creates a new Producer using the given client. It is still
// necessary to call Close() on the underlying client when shutting down this producer.
func NewAsyncProducerFromClient(client Client) (AsyncProducer, error) {
	// Check that we are not dealing with a closed Client before processing any other arguments
	if client.Closed() {
		return nil, ErrClosedClient
	}

	p := &asyncProducer{
		client:     client,
		conf:       client.Config(),
		errors:     make(chan *ProducerError),
		input:      make(chan *ProducerMessage),
		successes:  make(chan *ProducerMessage),
		retries:    make(chan *ProducerMessage),
		brokers:    make(map[*Broker]chan<- *ProducerMessage),
		brokerRefs: make(map[chan<- *ProducerMessage]int),
	}

	// launch our singleton dispatchers
	go withRecover(p.dispatcher)
	go withRecover(p.retryHandler)

	return p, nil
}

type flagSet int8

const (
	chaser   flagSet = 1 << iota // message is last in a group that failed
	shutdown                     // start the shutdown process
)

// ProducerMessage is the collection of elements passed to the Producer in order to send a message.
type ProducerMessage struct {
	Topic string // The Kafka topic for this message.
	// The partitioning key for this message. Pre-existing Encoders include
	// StringEncoder and ByteEncoder.
	Key Encoder
	// The actual message to store in Kafka. Pre-existing Encoders include
	// StringEncoder and ByteEncoder.
	Value Encoder

	// This field is used to hold arbitrary data you wish to include so it
	// will be available when receiving on the Successes and Errors channels.
	// Sarama completely ignores this field and is only to be used for
	// pass-through data.
	Metadata interface{}

	// Below this point are filled in by the producer as the message is processed

	// Offset is the offset of the message stored on the broker. This is only
	// guaranteed to be defined if the message was successfully delivered and
	// RequiredAcks is not NoResponse.
	Offset int64
	// Partition is the partition that the message was sent to. This is only
	// guaranteed to be defined if the message was successfully delivered.
	Partition int32

	retries int
	flags   flagSet

	keyCache, valueCache []byte
}

func (m *ProducerMessage) byteSize() int {
	size := 26 // the metadata overhead of CRC, flags, etc.
	if m.Key != nil {
		size += m.Key.Length()
	}
	if m.Value != nil {
		size += m.Value.Length()
	}
	return size
}

func (m *ProducerMessage) clear() {
	m.flags = 0
	m.retries = 0
	m.keyCache = nil
	m.valueCache = nil
}

// ProducerError is the type of error generated when the producer fails to deliver a message.
// It contains the original ProducerMessage as well as the actual error value.
type ProducerError struct {
	Msg *ProducerMessage
	Err error
}

func (pe ProducerError) Error() string {
	return fmt.Sprintf("kafka: Failed to produce message to topic %s: %s", pe.Msg.Topic, pe.Err)
}

// ProducerErrors is a type that wraps a batch of "ProducerError"s and implements the Error interface.
// It can be returned from the Producer's Close method to avoid the need to manually drain the Errors channel
// when closing a producer.
type ProducerErrors []*ProducerError

func (pe ProducerErrors) Error() string {
	return fmt.Sprintf("kafka: Failed to deliver %d messages.", len(pe))
}

func (p *asyncProducer) Errors() <-chan *ProducerError {
	return p.errors
}

func (p *asyncProducer) Successes() <-chan *ProducerMessage {
	return p.successes
}

func (p *asyncProducer) Input() chan<- *ProducerMessage {
	return p.input
}

func (p *asyncProducer) Close() error {
	p.AsyncClose()

	if p.conf.Producer.Return.Successes {
		go withRecover(func() {
			for _ = range p.successes {
			}
		})
	}

	var errors ProducerErrors
	if p.conf.Producer.Return.Errors {
		for event := range p.errors {
			errors = append(errors, event)
		}
	}

	if len(errors) > 0 {
		return errors
	}
	return nil
}

func (p *asyncProducer) AsyncClose() {
	go withRecover(p.shutdown)
}

// singleton
// dispatches messages by topic
func (p *asyncProducer) dispatcher() {
	handlers := make(map[string]chan<- *ProducerMessage)
	shuttingDown := false

	for msg := range p.input {
		if msg == nil {
			Logger.Println("Something tried to send a nil message, it was ignored.")
			continue
		}

		if msg.flags&shutdown != 0 {
			shuttingDown = true
			p.inFlight.Done()
			continue
		} else if msg.retries == 0 {
			if shuttingDown {
				// we can't just call returnError here because that decrements the wait group,
				// which hasn't been incremented yet for this message, and shouldn't be
				pErr := &ProducerError{Msg: msg, Err: ErrShuttingDown}
				if p.conf.Producer.Return.Errors {
					p.errors <- pErr
				} else {
					Logger.Println(pErr)
				}
				continue
			}
			p.inFlight.Add(1)
		}

		if (p.conf.Producer.Compression == CompressionNone && msg.Value != nil && msg.Value.Length() > p.conf.Producer.MaxMessageBytes) ||
			(msg.byteSize() > p.conf.Producer.MaxMessageBytes) {

			p.returnError(msg, ErrMessageSizeTooLarge)
			continue
		}

		handler := handlers[msg.Topic]
		if handler == nil {
			handler = p.newTopicProducer(msg.Topic)
			handlers[msg.Topic] = handler
		}

		handler <- msg
	}

	for _, handler := range handlers {
		close(handler)
	}
}

// one per topic
// partitions messages, then dispatches them by partition
type topicProducer struct {
	parent *asyncProducer
	topic  string
	input  <-chan *ProducerMessage

	breaker     *breaker.Breaker
	handlers    map[int32]chan<- *ProducerMessage
	partitioner Partitioner
}

func (p *asyncProducer) newTopicProducer(topic string) chan<- *ProducerMessage {
	input := make(chan *ProducerMessage, p.conf.ChannelBufferSize)
	tp := &topicProducer{
		parent:      p,
		topic:       topic,
		input:       input,
		breaker:     breaker.New(3, 1, 10*time.Second),
		handlers:    make(map[int32]chan<- *ProducerMessage),
		partitioner: p.conf.Producer.Partitioner(topic),
	}
	go withRecover(tp.dispatch)
	return input
}

func (tp *topicProducer) dispatch() {
	for msg := range tp.input {
		if msg.retries == 0 {
			if err := tp.partitionMessage(msg); err != nil {
				tp.parent.returnError(msg, err)
				continue
			}
		}

		handler := tp.handlers[msg.Partition]
		if handler == nil {
			handler = tp.parent.newPartitionProducer(msg.Topic, msg.Partition)
			tp.handlers[msg.Partition] = handler
		}

		handler <- msg
	}

	for _, handler := range tp.handlers {
		close(handler)
	}
}

func (tp *topicProducer) partitionMessage(msg *ProducerMessage) error {
	var partitions []int32

	err := tp.breaker.Run(func() (err error) {
		if tp.partitioner.RequiresConsistency() {
			partitions, err = tp.parent.client.Partitions(msg.Topic)
		} else {
			partitions, err = tp.parent.client.WritablePartitions(msg.Topic)
		}
		return
	})

	if err != nil {
		return err
	}

	numPartitions := int32(len(partitions))

	if numPartitions == 0 {
		return ErrLeaderNotAvailable
	}

	choice, err := tp.partitioner.Partition(msg, numPartitions)

	if err != nil {
		return err
	} else if choice < 0 || choice >= numPartitions {
		return ErrInvalidPartition
	}

	msg.Partition = partitions[choice]

	return nil
}

// one per partition per topic
// dispatches messages to the appropriate broker
// also responsible for maintaining message order during retries
type partitionProducer struct {
	parent    *asyncProducer
	topic     string
	partition int32
	input     <-chan *ProducerMessage

	leader  *Broker
	breaker *breaker.Breaker
	output  chan<- *ProducerMessage

	// highWatermark tracks the "current" retry level, which is the only one where we actually let messages through,
	// all other messages get buffered in retryState[msg.retries].buf to preserve ordering
	// retryState[msg.retries].expectChaser simply tracks whether we've seen a chaser message for a given level (and
	// therefore whether our buffer is complete and safe to flush)
	highWatermark int
	retryState    []partitionRetryState
}

type partitionRetryState struct {
	buf          []*ProducerMessage
	expectChaser bool
}

func (p *asyncProducer) newPartitionProducer(topic string, partition int32) chan<- *ProducerMessage {
	input := make(chan *ProducerMessage, p.conf.ChannelBufferSize)
	pp := &partitionProducer{
		parent:    p,
		topic:     topic,
		partition: partition,
		input:     input,

		breaker:    breaker.New(3, 1, 10*time.Second),
		retryState: make([]partitionRetryState, p.conf.Producer.Retry.Max+1),
	}
	go withRecover(pp.dispatch)
	return input
}

func (pp *partitionProducer) dispatch() {
	// try to prefetch the leader; if this doesn't work, we'll do a proper call to `updateLeader`
	// on the first message
	pp.leader, _ = pp.parent.client.Leader(pp.topic, pp.partition)
	if pp.leader != nil {
		pp.output = pp.parent.getBrokerProducer(pp.leader)
	}

	for msg := range pp.input {
		if msg.retries > pp.highWatermark {
			// a new, higher, retry level; handle it and then back off
			pp.newHighWatermark(msg.retries)
			time.Sleep(pp.parent.conf.Producer.Retry.Backoff)
		} else if pp.highWatermark > 0 {
			// we are retrying something (else highWatermark would be 0) but this message is not a *new* retry level
			if msg.retries < pp.highWatermark {
				// in fact this message is not even the current retry level, so buffer it for now (unless it's a just a chaser)
				if msg.flags&chaser == chaser {
					pp.retryState[msg.retries].expectChaser = false
					pp.parent.inFlight.Done() // this chaser is now handled and will be garbage collected
				} else {
					pp.retryState[msg.retries].buf = append(pp.retryState[msg.retries].buf, msg)
				}
				continue
			} else if msg.flags&chaser == chaser {
				// this message is of the current retry level (msg.retries == highWatermark) and the chaser flag is set,
				// meaning this retry level is done and we can go down (at least) one level and flush that
				pp.retryState[pp.highWatermark].expectChaser = false
				pp.flushRetryBuffers()
				pp.parent.inFlight.Done() // this chaser is now handled and will be garbage collected
				continue
			}
		}

		// if we made it this far then the current msg contains real data, and can be sent to the next goroutine
		// without breaking any of our ordering guarantees

		if pp.output == nil {
			if err := pp.updateLeader(); err != nil {
				pp.parent.returnError(msg, err)
				time.Sleep(pp.parent.conf.Producer.Retry.Backoff)
				continue
			}
			Logger.Printf("producer/leader/%s/%d selected broker %d\n", pp.topic, pp.partition, pp.leader.ID())
		}

		pp.output <- msg
	}

	if pp.output != nil {
		pp.parent.unrefBrokerProducer(pp.leader, pp.output)
	}
}

func (pp *partitionProducer) newHighWatermark(hwm int) {
	Logger.Printf("producer/leader/%s/%d state change to [retrying-%d]\n", pp.topic, pp.partition, hwm)
	pp.highWatermark = hwm

	// send off a chaser so that we know when everything "in between" has made it
	// back to us and we can safely flush the backlog (otherwise we risk re-ordering messages)
	pp.retryState[pp.highWatermark].expectChaser = true
	pp.parent.inFlight.Add(1) // we're generating a chaser message; track it so we don't shut down while it's still inflight
	pp.output <- &ProducerMessage{Topic: pp.topic, Partition: pp.partition, flags: chaser, retries: pp.highWatermark - 1}

	// a new HWM means that our current broker selection is out of date
	Logger.Printf("producer/leader/%s/%d abandoning broker %d\n", pp.topic, pp.partition, pp.leader.ID())
	pp.parent.unrefBrokerProducer(pp.leader, pp.output)
	pp.output = nil
}

func (pp *partitionProducer) flushRetryBuffers() {
	Logger.Printf("producer/leader/%s/%d state change to [flushing-%d]\n", pp.topic, pp.partition, pp.highWatermark)
	for {
		pp.highWatermark--

		if pp.output == nil {
			if err := pp.updateLeader(); err != nil {
				pp.parent.returnErrors(pp.retryState[pp.highWatermark].buf, err)
				goto flushDone
			}
			Logger.Printf("producer/leader/%s/%d selected broker %d\n", pp.topic, pp.partition, pp.leader.ID())
		}

		for _, msg := range pp.retryState[pp.highWatermark].buf {
			pp.output <- msg
		}

	flushDone:
		pp.retryState[pp.highWatermark].buf = nil
		if pp.retryState[pp.highWatermark].expectChaser {
			Logger.Printf("producer/leader/%s/%d state change to [retrying-%d]\n", pp.topic, pp.partition, pp.highWatermark)
			break
		} else if pp.highWatermark == 0 {
			Logger.Printf("producer/leader/%s/%d state change to [normal]\n", pp.topic, pp.partition)
			break
		}
	}
}

func (pp *partitionProducer) updateLeader() error {
	return pp.breaker.Run(func() (err error) {
		if err = pp.parent.client.RefreshMetadata(pp.topic); err != nil {
			return err
		}

		if pp.leader, err = pp.parent.client.Leader(pp.topic, pp.partition); err != nil {
			return err
		}

		pp.output = pp.parent.getBrokerProducer(pp.leader)
		return nil
	})
}

// one per broker, constructs both an aggregator and a flusher
func (p *asyncProducer) newBrokerProducer(broker *Broker) chan<- *ProducerMessage {
	input := make(chan *ProducerMessage)
	bridge := make(chan []*ProducerMessage)

	a := &aggregator{
		parent: p,
		broker: broker,
		input:  input,
		output: bridge,
	}
	go withRecover(a.run)

	f := &flusher{
		parent:         p,
		broker:         broker,
		input:          bridge,
		currentRetries: make(map[string]map[int32]error),
	}
	go withRecover(f.run)

	return input
}

// groups messages together into appropriately-sized batches for sending to the broker
// based on https://godoc.org/github.com/eapache/channels#BatchingChannel
type aggregator struct {
	parent *asyncProducer
	broker *Broker
	input  <-chan *ProducerMessage
	output chan<- []*ProducerMessage

	buffer      []*ProducerMessage
	bufferBytes int
	timer       <-chan time.Time
}

func (a *aggregator) run() {
	var output chan<- []*ProducerMessage

	for {
		select {
		case msg := <-a.input:
			if msg == nil {
				goto shutdown
			}

			if a.wouldOverflow(msg) {
				Logger.Printf("producer/aggregator/%d maximum request accumulated, forcing blocking flush\n", a.broker.ID())
				a.output <- a.buffer
				a.reset()
				output = nil
			}

			a.buffer = append(a.buffer, msg)
			a.bufferBytes += msg.byteSize()

			if a.readyToFlush(msg) {
				output = a.output
			} else if a.parent.conf.Producer.Flush.Frequency > 0 && a.timer == nil {
				a.timer = time.After(a.parent.conf.Producer.Flush.Frequency)
			}
		case <-a.timer:
			output = a.output
		case output <- a.buffer:
			a.reset()
			output = nil
		}
	}

shutdown:
	if len(a.buffer) > 0 {
		a.output <- a.buffer
	}
	close(a.output)
}

func (a *aggregator) wouldOverflow(msg *ProducerMessage) bool {
	switch {
	// Would we overflow our maximum possible size-on-the-wire? 10KiB is arbitrary overhead for safety.
	case a.bufferBytes+msg.byteSize() >= int(MaxRequestSize-(10*1024)):
		return true
	// Would we overflow the size-limit of a compressed message-batch?
	case a.parent.conf.Producer.Compression != CompressionNone && a.bufferBytes+msg.byteSize() >= a.parent.conf.Producer.MaxMessageBytes:
		return true
	// Would we overflow simply in number of messages?
	case a.parent.conf.Producer.Flush.MaxMessages > 0 && len(a.buffer) >= a.parent.conf.Producer.Flush.MaxMessages:
		return true
	default:
		return false
	}
}

func (a *aggregator) readyToFlush(msg *ProducerMessage) bool {
	switch {
	// If all three config values are 0, we always flush as-fast-as-possible
	case a.parent.conf.Producer.Flush.Frequency == 0 && a.parent.conf.Producer.Flush.Bytes == 0 && a.parent.conf.Producer.Flush.Messages == 0:
		return true
	// If the messages is a chaser we must flush to maintain the state-machine
	case msg.flags&chaser == chaser:
		return true
	// If we've  passed the message trigger-point
	case a.parent.conf.Producer.Flush.Messages > 0 && len(a.buffer) >= a.parent.conf.Producer.Flush.Messages:
		return true
	// If we've  passed the byte trigger-point
	case a.parent.conf.Producer.Flush.Bytes > 0 && a.bufferBytes >= a.parent.conf.Producer.Flush.Bytes:
		return true
	default:
		return false
	}
}

func (a *aggregator) reset() {
	a.timer = nil
	a.buffer = nil
	a.bufferBytes = 0
}

// takes a batch at a time from the aggregator and sends to the broker
type flusher struct {
	parent *asyncProducer
	broker *Broker
	input  <-chan []*ProducerMessage

	currentRetries map[string]map[int32]error
}

func (f *flusher) run() {
	var closing error

	Logger.Printf("producer/flusher/%d starting up\n", f.broker.ID())

	for batch := range f.input {
		if closing != nil {
			f.parent.retryMessages(batch, closing)
			continue
		}

		msgSets := f.groupAndFilter(batch)
		request := f.parent.buildRequest(msgSets)
		if request == nil {
			continue
		}

		response, err := f.broker.Produce(request)

		switch err.(type) {
		case nil:
			break
		case PacketEncodingError:
			f.parent.returnErrors(batch, err)
			continue
		default:
			Logger.Printf("producer/flusher/%d state change to [closing] because %s\n", f.broker.ID(), err)
			f.parent.abandonBrokerConnection(f.broker)
			_ = f.broker.Close()
			closing = err
			f.parent.retryMessages(batch, err)
			continue
		}

		if response == nil {
			// this only happens when RequiredAcks is NoResponse, so we have to assume success
			f.parent.returnSuccesses(batch)
			continue
		}

		f.parseResponse(msgSets, response)
	}
	Logger.Printf("producer/flusher/%d shut down\n", f.broker.ID())
}

func (f *flusher) groupAndFilter(batch []*ProducerMessage) map[string]map[int32][]*ProducerMessage {
	var err error
	msgSets := make(map[string]map[int32][]*ProducerMessage)

	for i, msg := range batch {

		if f.currentRetries[msg.Topic] != nil && f.currentRetries[msg.Topic][msg.Partition] != nil {
			// we're currently retrying this partition so we need to filter out this message
			f.parent.retryMessages([]*ProducerMessage{msg}, f.currentRetries[msg.Topic][msg.Partition])
			batch[i] = nil

			if msg.flags&chaser == chaser {
				// ...but now we can start processing future messages again
				Logger.Printf("producer/flusher/%d state change to [normal] on %s/%d\n",
					f.broker.ID(), msg.Topic, msg.Partition)
				delete(f.currentRetries[msg.Topic], msg.Partition)
			}

			continue
		}

		if msg.Key != nil {
			if msg.keyCache, err = msg.Key.Encode(); err != nil {
				f.parent.returnError(msg, err)
				batch[i] = nil
				continue
			}
		}

		if msg.Value != nil {
			if msg.valueCache, err = msg.Value.Encode(); err != nil {
				f.parent.returnError(msg, err)
				batch[i] = nil
				continue
			}
		}

		partitionSet := msgSets[msg.Topic]
		if partitionSet == nil {
			partitionSet = make(map[int32][]*ProducerMessage)
			msgSets[msg.Topic] = partitionSet
		}

		partitionSet[msg.Partition] = append(partitionSet[msg.Partition], msg)
	}

	return msgSets
}

func (f *flusher) parseResponse(msgSets map[string]map[int32][]*ProducerMessage, response *ProduceResponse) {
	// we iterate through the blocks in the request set, not the response, so that we notice
	// if the response is missing a block completely
	for topic, partitionSet := range msgSets {
		for partition, msgs := range partitionSet {
			block := response.GetBlock(topic, partition)
			if block == nil {
				f.parent.returnErrors(msgs, ErrIncompleteResponse)
				continue
			}

			switch block.Err {
			// Success
			case ErrNoError:
				for i := range msgs {
					msgs[i].Offset = block.Offset + int64(i)
				}
				f.parent.returnSuccesses(msgs)
			// Retriable errors
			case ErrUnknownTopicOrPartition, ErrNotLeaderForPartition, ErrLeaderNotAvailable,
				ErrRequestTimedOut, ErrNotEnoughReplicas, ErrNotEnoughReplicasAfterAppend:
				Logger.Printf("producer/flusher/%d state change to [retrying] on %s/%d because %v\n",
					f.broker.ID(), topic, partition, block.Err)
				if f.currentRetries[topic] == nil {
					f.currentRetries[topic] = make(map[int32]error)
				}
				f.currentRetries[topic][partition] = block.Err
				f.parent.retryMessages(msgs, block.Err)
			// Other non-retriable errors
			default:
				f.parent.returnErrors(msgs, block.Err)
			}
		}
	}
}

// singleton
// effectively a "bridge" between the flushers and the dispatcher in order to avoid deadlock
// based on https://godoc.org/github.com/eapache/channels#InfiniteChannel
func (p *asyncProducer) retryHandler() {
	var msg *ProducerMessage
	buf := queue.New()

	for {
		if buf.Length() == 0 {
			msg = <-p.retries
		} else {
			select {
			case msg = <-p.retries:
			case p.input <- buf.Peek().(*ProducerMessage):
				buf.Remove()
				continue
			}
		}

		if msg == nil {
			return
		}

		buf.Add(msg)
	}
}

// utility functions

func (p *asyncProducer) shutdown() {
	Logger.Println("Producer shutting down.")
	p.inFlight.Add(1)
	p.input <- &ProducerMessage{flags: shutdown}

	p.inFlight.Wait()

	if p.ownClient {
		err := p.client.Close()
		if err != nil {
			Logger.Println("producer/shutdown failed to close the embedded client:", err)
		}
	}

	close(p.input)
	close(p.retries)
	close(p.errors)
	close(p.successes)
}

func (p *asyncProducer) buildRequest(batch map[string]map[int32][]*ProducerMessage) *ProduceRequest {

	req := &ProduceRequest{RequiredAcks: p.conf.Producer.RequiredAcks, Timeout: int32(p.conf.Producer.Timeout / time.Millisecond)}
	empty := true

	for topic, partitionSet := range batch {
		for partition, msgSet := range partitionSet {
			setToSend := new(MessageSet)
			setSize := 0
			for _, msg := range msgSet {
				if p.conf.Producer.Compression != CompressionNone && setSize+msg.byteSize() > p.conf.Producer.MaxMessageBytes {
					// compression causes message-sets to be wrapped as single messages, which have tighter
					// size requirements, so we have to respect those limits
					valBytes, err := encode(setToSend)
					if err != nil {
						Logger.Println(err) // if this happens, it's basically our fault.
						panic(err)
					}
					req.AddMessage(topic, partition, &Message{Codec: p.conf.Producer.Compression, Key: nil, Value: valBytes})
					setToSend = new(MessageSet)
					setSize = 0
				}
				setSize += msg.byteSize()

				setToSend.addMessage(&Message{Codec: CompressionNone, Key: msg.keyCache, Value: msg.valueCache})
				empty = false
			}

			if p.conf.Producer.Compression == CompressionNone {
				req.AddSet(topic, partition, setToSend)
			} else {
				valBytes, err := encode(setToSend)
				if err != nil {
					Logger.Println(err) // if this happens, it's basically our fault.
					panic(err)
				}
				req.AddMessage(topic, partition, &Message{Codec: p.conf.Producer.Compression, Key: nil, Value: valBytes})
			}
		}
	}

	if empty {
		return nil
	}
	return req
}

func (p *asyncProducer) returnError(msg *ProducerMessage, err error) {
	msg.clear()
	pErr := &ProducerError{Msg: msg, Err: err}
	if p.conf.Producer.Return.Errors {
		p.errors <- pErr
	} else {
		Logger.Println(pErr)
	}
	p.inFlight.Done()
}

func (p *asyncProducer) returnErrors(batch []*ProducerMessage, err error) {
	for _, msg := range batch {
		if msg != nil {
			p.returnError(msg, err)
		}
	}
}

func (p *asyncProducer) returnSuccesses(batch []*ProducerMessage) {
	for _, msg := range batch {
		if msg == nil {
			continue
		}
		if p.conf.Producer.Return.Successes {
			msg.clear()
			p.successes <- msg
		}
		p.inFlight.Done()
	}
}

func (p *asyncProducer) retryMessages(batch []*ProducerMessage, err error) {
	for _, msg := range batch {
		if msg == nil {
			continue
		}
		if msg.retries >= p.conf.Producer.Retry.Max {
			p.returnError(msg, err)
		} else {
			msg.retries++
			p.retries <- msg
		}
	}
}

func (p *asyncProducer) getBrokerProducer(broker *Broker) chan<- *ProducerMessage {
	p.brokerLock.Lock()
	defer p.brokerLock.Unlock()

	bp := p.brokers[broker]

	if bp == nil {
		bp = p.newBrokerProducer(broker)
		p.brokers[broker] = bp
		p.brokerRefs[bp] = 0
	}

	p.brokerRefs[bp]++

	return bp
}

func (p *asyncProducer) unrefBrokerProducer(broker *Broker, bp chan<- *ProducerMessage) {
	p.brokerLock.Lock()
	defer p.brokerLock.Unlock()

	p.brokerRefs[bp]--
	if p.brokerRefs[bp] == 0 {
		close(bp)
		delete(p.brokerRefs, bp)

		if p.brokers[broker] == bp {
			delete(p.brokers, broker)
		}
	}
}

func (p *asyncProducer) abandonBrokerConnection(broker *Broker) {
	p.brokerLock.Lock()
	defer p.brokerLock.Unlock()

	delete(p.brokers, broker)
}
