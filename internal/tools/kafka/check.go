package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const CheckName = "kafka_check"

// maxGroupsInspected 限制逐组查询 lag 的数量，防止组爆炸时诊断被拖垮。
const maxGroupsInspected = 8

type CheckArgs struct {
	Addr        string `json:"addr"`
	Topic       string `json:"topic"`
	GroupPrefix string `json:"group_prefix,omitempty"`
}

type CheckTool struct{}

func NewCheck() *CheckTool { return &CheckTool{} }

func (t *CheckTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        CheckName,
		Description: "Read-only Kafka health check: broker liveness, cluster id, topic existence and partition count, consumer groups matching a prefix, and per-group lag. A missing topic or growing lag is reported as evidence, not a tool error.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"addr":         {Type: "string", Required: true, Description: "Kafka broker address such as localhost:29092."},
				"topic":        {Type: "string", Required: true, Description: "Topic to inspect, for example chat.events."},
				"group_prefix": {Type: "string", Description: "Optional consumer group prefix filter, for example chat-gateway-."},
			},
		},
	}
}

func (t *CheckTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args CheckArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode kafka_check args: %w", err)
	}
	args.Addr = strings.TrimSpace(args.Addr)
	if args.Addr == "" {
		return schema.Observation{}, fmt.Errorf("kafka_check requires addr")
	}
	args.Topic = strings.TrimSpace(args.Topic)
	if args.Topic == "" {
		return schema.Observation{}, fmt.Errorf("kafka_check requires topic")
	}

	startedAt := time.Now()
	transport, err := new(net.Dialer).DialContext(ctx, "tcp", args.Addr)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("connect kafka: %w", err)
	}
	defer transport.Close()
	tools.ApplyConnDeadline(ctx, transport)
	broker := &conn{transport: transport}

	// ApiVersions 区分"TCP 通"与"broker 真在说 Kafka 协议且可服务"，
	// 防止冒名端点或选主中的 broker 被误判为健康。
	if err := broker.apiVersions(); err != nil {
		return schema.Observation{}, fmt.Errorf("kafka ApiVersions handshake: %w", err)
	}

	metadata, err := broker.metadata(args.Topic)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("kafka metadata: %w", err)
	}
	topicExists := metadata.errorCode == 0 && len(metadata.partitions) > 0

	data := map[string]any{
		"addr":         args.Addr,
		"cluster_id":   metadata.clusterID,
		"broker_count": metadata.brokerCount,
		"topic":        args.Topic,
		"topic_exists": topicExists,
		"partitions":   len(metadata.partitions),
	}
	if !topicExists && metadata.errorCode != 0 {
		data["topic_error_code"] = metadata.errorCode
	}

	groups, err := broker.listGroups()
	if err != nil {
		return schema.Observation{}, fmt.Errorf("kafka list groups: %w", err)
	}
	prefix := strings.TrimSpace(args.GroupPrefix)
	matched := make([]string, 0, len(groups))
	for _, group := range groups {
		if prefix == "" || strings.HasPrefix(group, prefix) {
			matched = append(matched, group)
		}
	}
	sort.Strings(matched)
	data["groups_matched"] = len(matched)
	inspected := matched
	if len(inspected) > maxGroupsInspected {
		inspected = inspected[:maxGroupsInspected]
		data["groups_truncated"] = true
	}

	// 组状态用于区分活跃组和重启遗留的空组：空组的位点永远停滞，
	// 其"lag"是历史噪声，不能与活跃组的消费停滞混为一谈。
	states, err := broker.describeGroups(inspected)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("kafka describe groups: %w", err)
	}

	activeLag := int64(0)
	activeGroups := 0
	staleGroups := 0
	groupSummaries := make([]map[string]any, 0, len(inspected))
	if topicExists && len(inspected) > 0 {
		endOffsets, err := broker.endOffsets(args.Topic, metadata.partitions)
		if err != nil {
			return schema.Observation{}, fmt.Errorf("kafka list offsets: %w", err)
		}
		for _, group := range inspected {
			state := states[group]
			active := state.members > 0
			if active {
				activeGroups++
			} else {
				staleGroups++
			}
			committed, err := broker.committedOffsets(group, args.Topic, metadata.partitions)
			if err != nil {
				groupSummaries = append(groupSummaries, map[string]any{
					"group": group,
					"state": state.state,
					"error": tools.RedactSensitive(err.Error()),
				})
				continue
			}
			lag := int64(0)
			committedPartitions := 0
			for partition, end := range endOffsets {
				offset, ok := committed[partition]
				if !ok || offset < 0 {
					// latest 位点消费组在无新消息前可能尚无提交，不计入 lag。
					continue
				}
				committedPartitions++
				if end > offset {
					lag += end - offset
				}
			}
			if active {
				activeLag += lag
			}
			groupSummaries = append(groupSummaries, map[string]any{
				"group":                group,
				"state":                state.state,
				"members":              state.members,
				"lag":                  lag,
				"committed_partitions": committedPartitions,
			})
		}
		data["groups"] = groupSummaries
		data["active_groups"] = activeGroups
		data["stale_groups"] = staleGroups
		data["active_lag"] = activeLag
	}

	latencyMS := time.Since(startedAt).Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}
	data["latency_ms"] = latencyMS

	topicPart := fmt.Sprintf("topic %s has %d partitions", args.Topic, len(metadata.partitions))
	if !topicExists {
		topicPart = fmt.Sprintf("topic %s does not exist (error code %d)", args.Topic, metadata.errorCode)
	}
	groupPart := fmt.Sprintf("%d active / %d stale consumer groups", activeGroups, staleGroups)
	if prefix != "" {
		groupPart = fmt.Sprintf("%d active / %d stale consumer groups matching %q", activeGroups, staleGroups, prefix)
	}
	summary := fmt.Sprintf("Kafka %s cluster %s: %s; %s, active lag %d in %dms",
		args.Addr, metadata.clusterID, topicPart, groupPart, activeLag, latencyMS)

	return schema.Observation{
		Tool:    CheckName,
		Summary: summary,
		Data:    data,
	}, nil
}
