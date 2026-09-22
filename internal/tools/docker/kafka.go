package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const KafkaComposeCheckName = "kafka_compose_check"

var partitionCountPattern = regexp.MustCompile(`PartitionCount:\s*(\d+)`)

// KafkaComposeCheckTool 在 Kafka 所在的 Compose 网络内部执行固定只读 CLI 命令。
// topic、consumerGroup 和容器名均来自运营者配置，模型没有可注入的命令或参数。
type KafkaComposeCheckTool struct {
	policy        *policy
	container     string
	topic         string
	consumerGroup string
}

// NewKafkaComposeCheck 创建仅适用于容器内 Kafka broker 的只读检查工具。
// 参数均来自已加载的配置；scopes 用于复用 Compose 动态容器白名单策略。
func NewKafkaComposeCheck(allowedContainers []string, container, topic, consumerGroup string, scopes ...Scope) *KafkaComposeCheckTool {
	return &KafkaComposeCheckTool{
		policy:        newPolicy(allowedContainers, runDocker, scopes...),
		container:     strings.TrimSpace(container),
		topic:         strings.TrimSpace(topic),
		consumerGroup: strings.TrimSpace(consumerGroup),
	}
}

func (t *KafkaComposeCheckTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: KafkaComposeCheckName,
		Description: fmt.Sprintf(
			"Read-only Kafka check inside configured Docker container %s for topic %s and consumer group %s. It reports topic partitions and committed consumer lag without exposing the broker to the host.",
			t.container, t.topic, t.consumerGroup,
		),
		Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{}},
	}
}

func (t *KafkaComposeCheckTool) Run(ctx context.Context, _ json.RawMessage) (schema.Observation, error) {
	if err := t.validateConfig(); err != nil {
		return schema.Observation{}, err
	}
	container, err := t.policy.validate(t.container)
	if err != nil {
		return schema.Observation{}, err
	}

	topicOutput, err := t.policy.run(ctx,
		"exec", container,
		"/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "127.0.0.1:9092",
		"--describe", "--topic", t.topic,
	)
	if err != nil {
		text := tools.RedactSensitive(strings.TrimSpace(string(topicOutput)))
		observation := schema.Observation{
			Tool:    KafkaComposeCheckName,
			Summary: fmt.Sprintf("Kafka topic check failed in Docker container %s", container),
			Data: map[string]any{
				"container": container,
				"topic":     t.topic,
				"output":    text,
			},
		}
		return observation, fmt.Errorf("describe Kafka topic %q in %q: %w: %s", t.topic, container, err, text)
	}

	partitions := parsePartitionCount(string(topicOutput))
	data := map[string]any{
		"container":        container,
		"topic":            t.topic,
		"topic_partitions": partitions,
		"consumer_group":   t.consumerGroup,
	}

	groupOutput, groupErr := t.policy.run(ctx,
		"exec", container,
		"/opt/kafka/bin/kafka-consumer-groups.sh", "--bootstrap-server", "127.0.0.1:9092",
		"--describe", "--group", t.consumerGroup,
	)
	if groupErr != nil {
		data["group_available"] = false
		data["group_error"] = tools.RedactSensitive(strings.TrimSpace(string(groupOutput)))
		return schema.Observation{
			Tool:    KafkaComposeCheckName,
			Summary: fmt.Sprintf("Kafka topic %s has %d partitions; consumer group %s is unavailable for lag inspection", t.topic, partitions, t.consumerGroup),
			Data:    data,
		}, nil
	}

	lag, rows, activeConsumers := parseConsumerGroupDescription(string(groupOutput))
	data["group_available"] = true
	data["group_rows"] = rows
	data["active_consumers"] = activeConsumers
	data["active_lag"] = lag
	return schema.Observation{
		Tool: KafkaComposeCheckName,
		Summary: fmt.Sprintf(
			"Kafka topic %s has %d partitions; consumer group %s has %d active consumers, %d partition rows, active lag %d",
			t.topic, partitions, t.consumerGroup, activeConsumers, rows, lag,
		),
		Data: data,
	}, nil
}

// validateConfig 拒绝异常配置，避免即使在无 shell 拼接的情况下也把错误目标交给 Docker CLI。
func (t *KafkaComposeCheckTool) validateConfig() error {
	for key, value := range map[string]string{
		"container":      t.container,
		"topic":          t.topic,
		"consumer_group": t.consumerGroup,
	} {
		if value == "" || len([]rune(value)) > 128 || strings.ContainsAny(value, "\r\n\t ") {
			return fmt.Errorf("kafka compose check requires a valid %s", key)
		}
	}
	return nil
}

// parsePartitionCount 从 kafka-topics --describe 的 topic 摘要中读取分区数。
func parsePartitionCount(output string) int {
	match := partitionCountPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0
	}
	partitions, err := strconv.Atoi(match[1])
	if err != nil || partitions < 0 {
		return 0
	}
	return partitions
}

// parseConsumerGroupDescription 读取 kafka-consumer-groups --describe 的表格输出。
// 只累计数字 LAG 列；没有提交位点的 "-" 保留为未观测，不伪造成零 lag。
func parseConsumerGroupDescription(output string) (lag int64, rows int, activeConsumers int) {
	lagIndex := -1
	consumerIndex := -1
	consumers := make(map[string]struct{})
	for _, rawLine := range strings.Split(output, "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "GROUP" {
			for index, field := range fields {
				switch field {
				case "LAG":
					lagIndex = index
				case "CONSUMER-ID":
					consumerIndex = index
				}
			}
			continue
		}
		if lagIndex < 0 || len(fields) <= lagIndex {
			continue
		}
		rows++
		if parsed, err := strconv.ParseInt(fields[lagIndex], 10, 64); err == nil && parsed > 0 {
			lag += parsed
		}
		if consumerIndex >= 0 && len(fields) > consumerIndex && fields[consumerIndex] != "-" {
			consumers[fields[consumerIndex]] = struct{}{}
		}
	}
	return lag, rows, len(consumers)
}
