package docker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestKafkaComposeCheckReadsFixedTopicAndGroup 验证 Kafka 检查只执行配置确定的只读命令。
func TestKafkaComposeCheckReadsFixedTopicAndGroup(t *testing.T) {
	var calls [][]string
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if strings.Contains(args[2], "kafka-topics.sh") {
			return []byte("Topic: chat-messages\tTopicId: abc\tPartitionCount: 64\tReplicationFactor: 1\tConfigs:"), nil
		}
		return []byte("GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG CONSUMER-ID HOST CLIENT-ID\n" +
			"go-chat-message-writers chat-messages 0 10 14 4 consumer-a /x client\n" +
			"go-chat-message-writers chat-messages 1 11 11 0 consumer-a /x client\n" +
			"go-chat-message-writers chat-messages 2 8 12 4 consumer-b /x client"), nil
	}
	tool := &KafkaComposeCheckTool{
		policy:        newPolicy([]string{"chat-kafka"}, runner),
		container:     "chat-kafka",
		topic:         "chat-messages",
		consumerGroup: "go-chat-message-writers",
	}

	observation, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("run Kafka Compose check: %v", err)
	}
	wantTopicCall := []string{"exec", "chat-kafka", "/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "127.0.0.1:9092", "--describe", "--topic", "chat-messages"}
	if !reflect.DeepEqual(calls[0], wantTopicCall) {
		t.Fatalf("topic command = %#v, want %#v", calls[0], wantTopicCall)
	}
	if got := observation.Data["topic_partitions"]; got != 64 {
		t.Fatalf("partitions = %#v, want 64", got)
	}
	if got := observation.Data["active_lag"]; got != int64(8) {
		t.Fatalf("active lag = %#v, want 8", got)
	}
	if got := observation.Data["active_consumers"]; got != 2 {
		t.Fatalf("active consumers = %#v, want 2", got)
	}
}

// TestKafkaComposeCheckPreservesTopicFailure 验证容器内 Kafka 命令失败仍保留输出证据。
func TestKafkaComposeCheckPreservesTopicFailure(t *testing.T) {
	runner := func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("broker unavailable"), errors.New("exit status 1")
	}
	tool := &KafkaComposeCheckTool{
		policy:        newPolicy([]string{"chat-kafka"}, runner),
		container:     "chat-kafka",
		topic:         "chat-messages",
		consumerGroup: "go-chat-message-writers",
	}

	observation, err := tool.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected topic check error")
	}
	if got := observation.Data["output"]; got != "broker unavailable" {
		t.Fatalf("failure output = %#v", got)
	}
}
