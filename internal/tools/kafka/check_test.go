package kafka

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
)

// fakeBroker 按 api_key 回放 canned 响应体，验证 wire 解析与 lag 计算。
func startFakeBroker(t *testing.T, responses map[int16][][]byte) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	served := map[int16]int{}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			sizeBytes := make([]byte, 4)
			if _, err := io.ReadFull(conn, sizeBytes); err != nil {
				return
			}
			payload := make([]byte, binary.BigEndian.Uint32(sizeBytes))
			if _, err := io.ReadFull(conn, payload); err != nil {
				return
			}
			apiKey := int16(binary.BigEndian.Uint16(payload[0:2]))
			correlation := payload[4:8]

			queue := responses[apiKey]
			index := served[apiKey]
			if index >= len(queue) {
				return
			}
			served[apiKey]++
			body := queue[index]
			frame := &writer{}
			frame.int32(int32(4 + len(body)))
			frame.buffer = append(frame.buffer, correlation...)
			frame.buffer = append(frame.buffer, body...)
			if _, err := conn.Write(frame.buffer); err != nil {
				return
			}
		}
	}()
	return listener.Addr().String()
}

func TestKafkaCheckReportsTopicGroupsAndActiveLag(t *testing.T) {
	apiVersions := &writer{}
	apiVersions.int16(0)
	apiVersions.int32(0)

	metadata := &writer{}
	metadata.int32(1) // broker 数
	metadata.int32(1)
	metadata.string("localhost")
	metadata.int32(29092)
	metadata.int16(-1) // rack 为 null
	metadata.string("test-cluster")
	metadata.int32(1) // controller 数
	metadata.int32(1) // topic 数
	metadata.int16(0)
	metadata.string("t")
	metadata.buffer = append(metadata.buffer, 0) // is_internal=false
	metadata.int32(2)                            // 分区数
	for _, id := range []int32{0, 1} {
		metadata.int16(0)
		metadata.int32(id)
		metadata.int32(1)
		metadata.int32(0) // 副本列表
		metadata.int32(0) // 同步副本列表
	}

	listGroups := &writer{}
	listGroups.int16(0)
	listGroups.int32(2)
	listGroups.string("app-1")
	listGroups.string("consumer")
	listGroups.string("app-old")
	listGroups.string("consumer")

	describeGroups := &writer{}
	describeGroups.int32(2)
	describeGroups.int16(0)
	describeGroups.string("app-1")
	describeGroups.string("Stable")
	describeGroups.string("consumer")
	describeGroups.string("range")
	describeGroups.int32(1) // 一个成员
	describeGroups.string("member-1")
	describeGroups.string("client")
	describeGroups.string("/127.0.0.1")
	describeGroups.int32(0) // 元数据字节数
	describeGroups.int32(0) // 分配信息字节数
	describeGroups.int16(0)
	describeGroups.string("app-old")
	describeGroups.string("Empty")
	describeGroups.string("consumer")
	describeGroups.string("")
	describeGroups.int32(0) // 无成员

	listOffsets := &writer{}
	listOffsets.int32(1)
	listOffsets.string("t")
	listOffsets.int32(2)
	listOffsets.int32(0)
	listOffsets.int16(0)
	listOffsets.int64(-1)
	listOffsets.int64(10)
	listOffsets.int32(1)
	listOffsets.int16(0)
	listOffsets.int64(-1)
	listOffsets.int64(5)

	offsetsApp1 := &writer{}
	offsetsApp1.int32(1)
	offsetsApp1.string("t")
	offsetsApp1.int32(2)
	offsetsApp1.int32(0)
	offsetsApp1.int64(5)
	offsetsApp1.int16(-1)
	offsetsApp1.int16(0)
	offsetsApp1.int32(1)
	offsetsApp1.int64(5)
	offsetsApp1.int16(-1)
	offsetsApp1.int16(0)

	offsetsAppOld := &writer{}
	offsetsAppOld.int32(1)
	offsetsAppOld.string("t")
	offsetsAppOld.int32(2)
	offsetsAppOld.int32(0)
	offsetsAppOld.int64(3)
	offsetsAppOld.int16(-1)
	offsetsAppOld.int16(0)
	offsetsAppOld.int32(1)
	offsetsAppOld.int64(-1) // 无提交
	offsetsAppOld.int16(-1)
	offsetsAppOld.int16(0)

	addr := startFakeBroker(t, map[int16][][]byte{
		apiKeyAPIVersions:    {apiVersions.buffer},
		apiKeyMetadata:       {metadata.buffer},
		apiKeyListGroups:     {listGroups.buffer},
		apiKeyDescribeGroups: {describeGroups.buffer},
		apiKeyListOffsets:    {listOffsets.buffer},
		apiKeyOffsetFetch:    {offsetsApp1.buffer, offsetsAppOld.buffer},
	})

	observation, err := NewCheck().Run(context.Background(), json.RawMessage(
		`{"addr":"`+addr+`","topic":"t","group_prefix":"app-"}`))
	if err != nil {
		t.Fatalf("run kafka check: %v", err)
	}

	if observation.Data["topic_exists"] != true || observation.Data["partitions"] != 2 {
		t.Fatalf("topic data = %#v", observation.Data)
	}
	if observation.Data["cluster_id"] != "test-cluster" {
		t.Fatalf("cluster_id = %#v", observation.Data["cluster_id"])
	}
	if observation.Data["active_groups"] != 1 || observation.Data["stale_groups"] != 1 {
		t.Fatalf("group counts = %#v / %#v", observation.Data["active_groups"], observation.Data["stale_groups"])
	}
	// app-1 活跃：(10-5)+(5-5)=5；app-old 空组的 lag 不计入 active_lag。
	if observation.Data["active_lag"] != int64(5) {
		t.Fatalf("active_lag = %#v, want 5", observation.Data["active_lag"])
	}
	if !strings.Contains(observation.Summary, "1 active / 1 stale") || !strings.Contains(observation.Summary, "active lag 5") {
		t.Fatalf("summary = %q", observation.Summary)
	}
}

func TestKafkaCheckRejectsNonKafkaEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		// 冒名端点：直接回一段非 Kafka 帧后关闭。
		_, _ = conn.Write([]byte("+PONG\r\n"))
		conn.Close()
	}()

	_, err = NewCheck().Run(context.Background(), json.RawMessage(
		`{"addr":"`+listener.Addr().String()+`","topic":"t"}`))
	if err == nil || !strings.Contains(err.Error(), "ApiVersions") {
		t.Fatalf("error = %v, want ApiVersions handshake failure", err)
	}
}
