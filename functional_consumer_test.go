//go:build functional
// +build functional

package sarama

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFuncConsumerOffsetOutOfRange(t *testing.T) {
	setupFunctionalTest(t)
	defer teardownFunctionalTest(t)

	consumer, err := NewConsumer(FunctionalTestEnv.KafkaBrokerAddrs, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := consumer.ConsumePartition("test.1", 0, -10); !errors.Is(err, ErrOffsetOutOfRange) {
		t.Error("Expected ErrOffsetOutOfRange, got:", err)
	}

	if _, err := consumer.ConsumePartition("test.1", 0, math.MaxInt64); !errors.Is(err, ErrOffsetOutOfRange) {
		t.Error("Expected ErrOffsetOutOfRange, got:", err)
	}

	safeClose(t, consumer)
}

func TestConsumerHighWaterMarkOffset(t *testing.T) {
	setupFunctionalTest(t)
	defer teardownFunctionalTest(t)

	p, err := NewSyncProducer(FunctionalTestEnv.KafkaBrokerAddrs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer safeClose(t, p)

	_, offset, err := p.SendMessage(&ProducerMessage{Topic: "test.1", Value: StringEncoder("Test")})
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewConsumer(FunctionalTestEnv.KafkaBrokerAddrs, NewTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer safeClose(t, c)

	pc, err := c.ConsumePartition("test.1", 0, offset)
	if err != nil {
		t.Fatal(err)
	}

	<-pc.Messages()

	if hwmo := pc.HighWaterMarkOffset(); hwmo != offset+1 {
		t.Logf("Last produced offset %d; high water mark should be one higher but found %d.", offset, hwmo)
	}

	safeClose(t, pc)
}

// Makes sure that messages produced by all supported client versions/
// compression codecs (except LZ4) combinations can be consumed by all
// supported consumer versions. It relies on the KAFKA_VERSION environment
// variable to provide the version of the test Kafka cluster.
//
// Note that LZ4 codec was introduced in v0.10.0.0 and therefore is excluded
// from this test case. It has a similar version matrix test case below that
// only checks versions from v0.10.0.0 until KAFKA_VERSION.
func TestVersionMatrix(t *testing.T) {
	setupFunctionalTest(t)
	defer teardownFunctionalTest(t)

	// Produce lot's of message with all possible combinations of supported
	// protocol versions and compressions for the except of LZ4.
	testVersions := versionRange(V0_8_2_0)
	allCodecsButLZ4 := []CompressionCodec{CompressionNone, CompressionGZIP, CompressionSnappy}
	producedMessages := produceMsgs(t, testVersions, allCodecsButLZ4, 17, 100, false)

	// When/Then
	consumeMsgs(t, testVersions, producedMessages)
}

// Support for LZ4 codec was introduced in v0.10.0.0 so a version matrix to
// test LZ4 should start with v0.10.0.0.
func TestVersionMatrixLZ4(t *testing.T) {
	setupFunctionalTest(t)
	defer teardownFunctionalTest(t)

	// Produce lot's of message with all possible combinations of supported
	// protocol versions starting with v0.10 (first where LZ4 was supported)
	// and all possible compressions.
	testVersions := versionRange(V0_10_0_0)
	allCodecs := []CompressionCodec{CompressionNone, CompressionGZIP, CompressionSnappy, CompressionLZ4}
	producedMessages := produceMsgs(t, testVersions, allCodecs, 17, 100, false)

	// When/Then
	consumeMsgs(t, testVersions, producedMessages)
}

// Support for zstd codec was introduced in v2.1.0.0
func TestVersionMatrixZstd(t *testing.T) {
	setupFunctionalTest(t)
	defer teardownFunctionalTest(t)

	// Produce lot's of message with all possible combinations of supported
	// protocol versions starting with v2.1.0.0 (first where zstd was supported)
	testVersions := versionRange(V2_1_0_0)
	allCodecs := []CompressionCodec{CompressionZSTD}
	producedMessages := produceMsgs(t, testVersions, allCodecs, 17, 100, false)

	// When/Then
	consumeMsgs(t, testVersions, producedMessages)
}

func TestVersionMatrixIdempotent(t *testing.T) {
	setupFunctionalTest(t)
	defer teardownFunctionalTest(t)

	// Produce lot's of message with all possible combinations of supported
	// protocol versions starting with v0.11 (first where idempotent was supported)
	testVersions := versionRange(V0_11_0_0)
	producedMessages := produceMsgs(t, testVersions, []CompressionCodec{CompressionNone}, 17, 100, true)

	// When/Then
	consumeMsgs(t, testVersions, producedMessages)
}

func TestReadOnlyAndAllCommittedMessages(t *testing.T) {
	t.Skip("TODO: TestReadOnlyAndAllCommittedMessages is periodically failing inexplicably.")
	checkKafkaVersion(t, "0.11.0")
	setupFunctionalTest(t)
	defer teardownFunctionalTest(t)

	config := NewTestConfig()
	config.ClientID = t.Name()
	config.Net.MaxOpenRequests = 1
	config.Consumer.IsolationLevel = ReadCommitted
	config.Producer.Idempotent = true
	config.Producer.Return.Successes = true
	config.Producer.RequiredAcks = WaitForAll
	config.Version = V0_11_0_0

	client, err := NewClient(FunctionalTestEnv.KafkaBrokerAddrs, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	controller, err := client.Controller()
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	transactionalID := strconv.FormatInt(time.Now().UnixNano()/(1<<22), 10)

	var coordinator *Broker

	// find the transaction coordinator
	for {
		coordRes, err := controller.FindCoordinator(&FindCoordinatorRequest{
			Version:         2,
			CoordinatorKey:  transactionalID,
			CoordinatorType: CoordinatorTransaction,
		})
		if err != nil {
			t.Fatal(err)
		}
		if coordRes.Err != ErrNoError {
			continue
		}
		if err := coordRes.Coordinator.Open(client.Config()); err != nil {
			t.Fatal(err)
		}
		coordinator = coordRes.Coordinator
		break
	}

	// produce some uncommitted messages to the topic
	pidRes, err := coordinator.InitProducerID(&InitProducerIDRequest{
		TransactionalID:    &transactionalID,
		TransactionTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = coordinator.AddPartitionsToTxn(&AddPartitionsToTxnRequest{
		TransactionalID: transactionalID,
		ProducerID:      pidRes.ProducerID,
		ProducerEpoch:   pidRes.ProducerEpoch,
		TopicPartitions: map[string][]int32{
			uncommittedTopic: {0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ps := &produceSet{
		msgs: make(map[string]map[int32]*partitionSet),
		parent: &asyncProducer{
			conf: config,
		},
		producerID:    pidRes.ProducerID,
		producerEpoch: pidRes.ProducerEpoch,
	}
	_ = ps.add(&ProducerMessage{
		Topic:     uncommittedTopic,
		Partition: 0,
		Value:     StringEncoder("uncommitted message 1"),
	})
	_ = ps.add(&ProducerMessage{
		Topic:     uncommittedTopic,
		Partition: 0,
		Value:     StringEncoder("uncommitted message 2"),
	})
	produceReq := ps.buildRequest()
	produceReq.TransactionalID = &transactionalID
	if resp, err := coordinator.Produce(produceReq); err != nil {
		t.Fatal(err)
	} else {
		b := resp.GetBlock(uncommittedTopic, 0)
		if b != nil {
			t.Logf("uncommitted message 1 to %s-%d at offset %d", uncommittedTopic, 0, b.Offset)
			t.Logf("uncommitted message 2 to %s-%d at offset %d", uncommittedTopic, 0, b.Offset+1)
		}
	}

	// now produce some committed messages to the topic
	producer, err := NewAsyncProducer(FunctionalTestEnv.KafkaBrokerAddrs, config)
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()

	for i := 1; i <= 6; i++ {
		producer.Input() <- &ProducerMessage{
			Topic:     uncommittedTopic,
			Partition: 0,
			Value:     StringEncoder(fmt.Sprintf("Committed %v", i)),
		}
		msg := <-producer.Successes()
		t.Logf("Committed %v to %s-%d at offset %d", i, msg.Topic, msg.Partition, msg.Offset)
	}

	// now abort the uncommitted transaction
	if _, err := coordinator.EndTxn(&EndTxnRequest{
		TransactionalID:   transactionalID,
		ProducerID:        pidRes.ProducerID,
		ProducerEpoch:     pidRes.ProducerEpoch,
		TransactionResult: false, // aborted
	}); err != nil {
		t.Fatal(err)
	}

	consumer, err := NewConsumer(FunctionalTestEnv.KafkaBrokerAddrs, config)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()

	pc, err := consumer.ConsumePartition(uncommittedTopic, 0, OffsetOldest)
	require.NoError(t, err)

	msgChannel := pc.Messages()
	for i := 1; i <= 6; i++ {
		msg := <-msgChannel
		t.Logf("Received %s from %s-%d at offset %d", msg.Value, msg.Topic, msg.Partition, msg.Offset)
		require.Equal(t, fmt.Sprintf("Committed %v", i), string(msg.Value))
	}
}

func prodMsg2Str(prodMsg *ProducerMessage) string {
	return fmt.Sprintf("{offset: %d, value: %s}", prodMsg.Offset, string(prodMsg.Value.(StringEncoder)))
}

func consMsg2Str(consMsg *ConsumerMessage) string {
	return fmt.Sprintf("{offset: %d, value: %s}", consMsg.Offset, string(consMsg.Value))
}

func versionRange(lower KafkaVersion) []KafkaVersion {
	// Get the test cluster version from the environment. If there is nothing
	// there then assume the highest.
	upper, err := ParseKafkaVersion(os.Getenv("KAFKA_VERSION"))
	if err != nil {
		upper = MaxVersion
	}

	versions := make([]KafkaVersion, 0, len(SupportedVersions))
	for _, v := range SupportedVersions {
		if !v.IsAtLeast(lower) {
			continue
		}
		if !upper.IsAtLeast(v) {
			return versions
		}
		versions = append(versions, v)
	}
	return versions
}

func produceMsgs(t *testing.T, clientVersions []KafkaVersion, codecs []CompressionCodec, flush int, countPerVerCodec int, idempotent bool) []*ProducerMessage {
	var wg sync.WaitGroup
	var producedMessagesMu sync.Mutex
	var producedMessages []*ProducerMessage
	for _, prodVer := range clientVersions {
		for _, codec := range codecs {
			prodCfg := NewTestConfig()
			prodCfg.Version = prodVer
			prodCfg.Producer.Return.Successes = true
			prodCfg.Producer.Return.Errors = true
			prodCfg.Producer.Flush.MaxMessages = flush
			prodCfg.Producer.Compression = codec
			prodCfg.Producer.Idempotent = idempotent
			if idempotent {
				prodCfg.Producer.RequiredAcks = WaitForAll
				prodCfg.Net.MaxOpenRequests = 1
			}

			p, err := NewSyncProducer(FunctionalTestEnv.KafkaBrokerAddrs, prodCfg)
			if err != nil {
				t.Errorf("Failed to create producer: version=%s, compression=%s, err=%v", prodVer, codec, err)
				continue
			}
			defer safeClose(t, p)
			for i := 0; i < countPerVerCodec; i++ {
				msg := &ProducerMessage{
					Topic: "test.1",
					Value: StringEncoder(fmt.Sprintf("msg:%s:%s:%d", prodVer, codec, i)),
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _, err := p.SendMessage(msg)
					if err != nil {
						t.Errorf("Failed to produce message: %s, err=%v", msg.Value, err)
					}
					producedMessagesMu.Lock()
					producedMessages = append(producedMessages, msg)
					producedMessagesMu.Unlock()
				}()
			}
		}
	}
	wg.Wait()

	// Sort produced message in ascending offset order.
	sort.Slice(producedMessages, func(i, j int) bool {
		return producedMessages[i].Offset < producedMessages[j].Offset
	})
	t.Logf("*** Total produced %d, firstOffset=%d, lastOffset=%d\n",
		len(producedMessages), producedMessages[0].Offset, producedMessages[len(producedMessages)-1].Offset)
	return producedMessages
}

func consumeMsgs(t *testing.T, clientVersions []KafkaVersion, producedMessages []*ProducerMessage) {
	// Consume all produced messages with all client versions supported by the
	// cluster.
	for _, consVer := range clientVersions {
		t.Run(consVer.String(), func(t *testing.T) {
			t.Logf("*** Consuming with client version %s\n", consVer)
			// Create a partition consumer that should start from the first produced
			// message.
			consCfg := NewTestConfig()
			consCfg.Version = consVer
			c, err := NewConsumer(FunctionalTestEnv.KafkaBrokerAddrs, consCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer safeClose(t, c)
			pc, err := c.ConsumePartition("test.1", 0, producedMessages[0].Offset)
			if err != nil {
				t.Fatal(err)
			}
			defer safeClose(t, pc)

			// Consume as many messages as there have been produced and make sure that
			// order is preserved.
			for i, prodMsg := range producedMessages {
				select {
				case consMsg := <-pc.Messages():
					if consMsg.Offset != prodMsg.Offset {
						t.Fatalf("Consumed unexpected offset: version=%s, index=%d, want=%s, got=%s",
							consVer, i, prodMsg2Str(prodMsg), consMsg2Str(consMsg))
					}
					if string(consMsg.Value) != string(prodMsg.Value.(StringEncoder)) {
						t.Fatalf("Consumed unexpected msg: version=%s, index=%d, want=%s, got=%s",
							consVer, i, prodMsg2Str(prodMsg), consMsg2Str(consMsg))
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("Timeout waiting for: index=%d, offset=%d, msg=%s", i, prodMsg.Offset, prodMsg.Value)
				}
			}
		})
	}
}
