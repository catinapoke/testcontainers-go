package kafka_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/stretchr/testify/require"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/kafka"
	"github.com/testcontainers/testcontainers-go/network"
)

func TestKafka_Basic(t *testing.T) {
	topic := "some-topic"

	ctx := context.Background()

	kafkaContainer, err := kafka.Run(ctx, "confluentinc/confluent-local:7.5.0", kafka.WithClusterID("kraftCluster"))
	testcontainers.CleanupContainer(t, kafkaContainer)
	require.NoError(t, err)

	assertAdvertisedListeners(t, kafkaContainer)

	if !strings.EqualFold(kafkaContainer.ClusterID, "kraftCluster") {
		t.Fatalf("expected clusterID to be %s, got %s", "kraftCluster", kafkaContainer.ClusterID)
	}

	// getBrokers {
	brokers, err := kafkaContainer.Brokers(ctx)
	// }
	require.NoError(t, err)

	config := sarama.NewConfig()
	client, err := sarama.NewConsumerGroup(brokers, "groupName", config)
	require.NoError(t, err)

	consumer, ready, done, cancel := NewTestKafkaConsumer(t)
	defer cancel()
	go func() {
		if err := client.Consume(context.Background(), []string{topic}, consumer); err != nil {
			cancel()
		}
	}()

	// wait for the consumer to be ready
	<-ready

	// perform assertions

	// set config to true because successfully delivered messages will be returned on the Successes channel
	config.Producer.Return.Successes = true

	producer, err := sarama.NewSyncProducer(brokers, config)
	require.NoError(t, err)

	_, _, err = producer.SendMessage(&sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder("key"),
		Value: sarama.StringEncoder("value"),
	})
	require.NoError(t, err)

	<-done

	if !strings.EqualFold(string(consumer.message.Key), "key") {
		t.Fatalf("expected key to be %s, got %s", "key", string(consumer.message.Key))
	}
	if !strings.EqualFold(string(consumer.message.Value), "value") {
		t.Fatalf("expected value to be %s, got %s", "value", string(consumer.message.Value))
	}
}

func TestKafka_invalidVersion(t *testing.T) {
	ctx := context.Background()

	ctr, err := kafka.Run(ctx, "confluentinc/confluent-local:6.3.3", kafka.WithClusterID("kraftCluster"))
	testcontainers.CleanupContainer(t, ctr)
	require.Error(t, err)
}

func TestKafka_networkConnectivity(t *testing.T) {
	ctx := context.Background()
	var err error

	const (
		// config
		topic_in  = "topic_in"
		topic_out = "topic_out"

		address = "kafka:9092"

		// test data
		key      = "wow"
		text_msg = "test-input-external"
	)

	Network, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// kafkaWithListener {
	KafkaContainer, err := kafka.Run(ctx,
		"confluentinc/confluent-local:7.6.1",
		kafka.WithClusterID("test-cluster"),
		network.WithNetwork([]string{"kafka"}, Network),
		kafka.WithListener([]kafka.KafkaListener{
			{
				Name: "BROKER",
				Host: "kafka",
				Port: "9092",
			},
		}),
	)
	// }
	require.NoError(t, err, "failed to start kafka container")

	kcat, err := createKCat(ctx, Network.Name, "/tmp/msgs.txt")
	require.NoError(t, err, "failed to start kcat")

	// 4. Copy message to kcat
	err = kcat.CopyToContainer(ctx, []byte("Message produced by kcat"), kcat.FilePath, 700)
	require.NoError(t, err)

	brokers, err := KafkaContainer.Brokers(context.TODO())
	require.NoError(t, err, "failed to get brokers")

	// err = createTopics(brokers, []string{topic_in, topic_out})
	_, stdout, err := kcat.Exec(ctx, []string{"kcat", "-b", address, "-C", "-t", topic_in})
	require.NoError(t, err, "create topic topic_in")

	_, stdout, err = kcat.Exec(ctx, []string{"kcat", "-b", address, "-C", "-t", topic_out})
	require.NoError(t, err, "create topic topic_out")

	// perform assertions

	// set config to true because successfully delivered messages will be returned on the Successes channel
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	config.Consumer.MaxWaitTime = 2 * time.Second

	producer, err := sarama.NewSyncProducer(brokers, config)
	require.NoError(t, err, "create kafka producer")

	// Act

	// External write
	_, _, err = producer.SendMessage(&sarama.ProducerMessage{
		Topic: topic_in,
		Key:   sarama.StringEncoder(key),
		Value: sarama.StringEncoder(text_msg),
	})
	require.NoError(t, err, "send message")

	// Internal read
	_, stdout, err = kcat.Exec(ctx, []string{"kcat", "-b", address, "-C", "-t", topic_in, "-c", "1"})
	require.NoError(t, err)

	out, err := io.ReadAll(stdout)
	require.NoError(t, err, "read message in kcat")
	require.Contains(t, string(out), text_msg)

	// Internal write
	tempfile := "/tmp/msgs.txt"

	err = kcat.CopyToContainer(ctx, []byte(out), tempfile, 700)
	require.NoError(t, err)

	_, _, err = kcat.Exec(ctx, []string{"kcat", "-b", address, "-t", topic_out, "-P", "-l", tempfile})
	require.NoError(t, err, "send message with kcat")

	// External read
	client, err := sarama.NewConsumerGroup(brokers, "groupName", config)
	require.NoError(t, err, "create consumer group")

	consumer, _, done, cancel := NewTestKafkaConsumer(t)

	sCtx, _ := context.WithTimeout(context.Background(), time.Second)
	go func() {
		if err := client.Consume(sCtx, []string{topic_out}, consumer); err != nil {
			cancel()
		}
	}()

	// wait for the consumer to receive message
	select {
	case <-sCtx.Done():
		t.Log("exit by timeout")
	case <-done:
	}

	if consumer.message == nil {
		t.Fatal("Empty message")
	}

	// Assert
	if !strings.Contains(string(consumer.message.Value), text_msg) {
		t.Error("got wrong string")
	}
}

func TestKafka_withListener(t *testing.T) {
	ctx := context.Background()

	// 1. Create network
	rpNetwork, err := network.New(ctx)
	require.NoError(t, err)

	// 2. Start Kafka ctr
	// withListenerRP {
	ctr, err := kafka.Run(ctx,
		"confluentinc/confluent-local:7.6.1",
		network.WithNetwork([]string{"kafka"}, rpNetwork),
		kafka.WithListener([]kafka.KafkaListener{
			{
				Name: "BROKER",
				Host: "kafka",
				Port: "9092",
			},
		}),
	)
	// }
	require.NoError(t, err)

	// 3. Start KCat container
	kcat, err := createKCat(ctx, rpNetwork.Name, "/tmp/msgs.txt")
	require.NoError(t, err)

	// 4. Copy message to kcat
	err = kcat.CopyToContainer(ctx, []byte("Message produced by kcat"), kcat.FilePath, 700)
	require.NoError(t, err)

	// 5. Produce message to Kafka
	// withListenerExec {
	_, _, err = kcat.Exec(ctx, []string{"kcat", "-b", "kafka:9092", "-t", "msgs", "-P", "-l", kcat.FilePath})
	// }

	require.NoError(t, err)

	// 6. Consume message from Kafka
	_, stdout, err := kcat.Exec(ctx, []string{"kcat", "-b", "kafka:9092", "-C", "-t", "msgs", "-c", "1"})
	require.NoError(t, err)

	// 7. Read Message from stdout
	out, err := io.ReadAll(stdout)
	require.NoError(t, err)

	require.Contains(t, string(out), "Message produced by kcat")

	t.Cleanup(func() {
		if err := kcat.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate kcat container: %s", err)
		}
		if err := ctr.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate Kafka container: %s", err)
		}

		if err := rpNetwork.Remove(ctx); err != nil {
			t.Fatalf("failed to remove network: %s", err)
		}
	})
}

func TestKafka_restProxyService(t *testing.T) {
	// TODO: test kafka rest proxy service
}

func TestKafka_listenersValidation(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name      string
		listeners []kafka.KafkaListener
	}{
		{
			name: "reserved listener port duplication 1",
			listeners: []kafka.KafkaListener{
				{
					Name: "BROKER",
					Host: "kafka",
					Port: "9093",
				},
			},
		},
		{
			name: "reserved listener port duplication 2",
			listeners: []kafka.KafkaListener{
				{
					Name: "BROKER",
					Host: "kafka",
					Port: "9094",
				},
			},
		},
		{
			name: "reserved listener name duplication (controller)",
			listeners: []kafka.KafkaListener{
				{
					Name: "  cOnTrOller   ",
					Host: "kafka",
					Port: "9092",
				},
			},
		},
		{
			name: "reserved listener name duplication (plaintext)",
			listeners: []kafka.KafkaListener{
				{
					Name: "plaintext",
					Host: "kafka",
					Port: "9092",
				},
			},
		},
		{
			name: "duplicated ports not allowed",
			listeners: []kafka.KafkaListener{
				{
					Name: "test",
					Host: "kafka",
					Port: "9092",
				},
				{
					Name: "test2",
					Host: "kafka",
					Port: "9092",
				},
			},
		},
		{
			name: "duplicated names not allowed",
			listeners: []kafka.KafkaListener{
				{
					Name: "test",
					Host: "kafka",
					Port: "9092",
				},
				{
					Name: "test",
					Host: "kafka",
					Port: "9095",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := kafka.Run(ctx,
				"confluentinc/confluent-local:7.6.1",
				kafka.WithClusterID("test-cluster"),
				kafka.WithListener(tc.listeners),
			)
			require.Error(t, err)
			require.Nil(t, c, "expected container to be nil")
		})
	}
}

func createTopics(brokers []string, topics []string) error {
	t := &sarama.CreateTopicsRequest{}
	t.TopicDetails = make(map[string]*sarama.TopicDetail, len(topics))
	for _, elem := range topics {
		t.TopicDetails[elem] = &sarama.TopicDetail{NumPartitions: 1}
	}

	var err error

	c, err := sarama.NewClient(brokers, sarama.NewConfig())
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer c.Close()

	bs := c.Brokers()

	_, err = bs[0].CreateTopics(t)
	if err != nil {
		return fmt.Errorf("failed to create topics: %w", err)
	}

	fmt.Println("successfully created topics")

	return nil
}

// assertAdvertisedListeners checks that the advertised listeners are set correctly:
// - The BROKER:// protocol is using the hostname of the Kafka container
func assertAdvertisedListeners(t *testing.T, container testcontainers.Container) {
	inspect, err := container.Inspect(context.Background())
	require.NoError(t, err)

	hostname := inspect.Config.Hostname

	code, r, err := container.Exec(context.Background(), []string{"cat", "/usr/sbin/testcontainers_start.sh"})
	require.NoError(t, err)

	require.Zero(t, code)

	bs, err := io.ReadAll(r)
	require.NoError(t, err)

	if !strings.Contains(string(bs), "BROKER://"+hostname+":9092") {
		t.Fatalf("expected advertised listeners to contain %s, got %s", "BROKER://"+hostname+":9092", string(bs))
	}
}
