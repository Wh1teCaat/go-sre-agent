// kafka 包实现只读 Kafka 诊断所需的最小线协议子集。
// 与 postgres_ping 手写 startup message 同一取舍：只覆盖诊断需要的少数
// 旧版本 API（均为非 flexible 编码），不引入完整客户端依赖。
package kafka

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const (
	apiKeyListOffsets    int16 = 2
	apiKeyMetadata       int16 = 3
	apiKeyOffsetFetch    int16 = 9
	apiKeyDescribeGroups int16 = 15
	apiKeyListGroups     int16 = 16
	apiKeyAPIVersions    int16 = 18

	clientID = "go-sre-agent"

	// maxResponseBytes 限制单个响应帧大小，异常 broker 不能把内存打满。
	maxResponseBytes = 4 << 20
)

// writer 按 Kafka 协议编码大端字段。
type writer struct {
	buffer []byte
}

func (w *writer) int16(value int16) {
	w.buffer = binary.BigEndian.AppendUint16(w.buffer, uint16(value))
}

func (w *writer) int32(value int32) {
	w.buffer = binary.BigEndian.AppendUint32(w.buffer, uint32(value))
}

func (w *writer) int64(value int64) {
	w.buffer = binary.BigEndian.AppendUint64(w.buffer, uint64(value))
}

func (w *writer) string(value string) {
	w.int16(int16(len(value)))
	w.buffer = append(w.buffer, value...)
}

// reader 按 Kafka 协议解码，出错后短路，最后统一检查 err。
type reader struct {
	buffer []byte
	offset int
	err    error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.offset+n > len(r.buffer) {
		r.err = fmt.Errorf("kafka response truncated at offset %d (+%d of %d)", r.offset, n, len(r.buffer))
		return nil
	}
	data := r.buffer[r.offset : r.offset+n]
	r.offset += n
	return data
}

func (r *reader) int16() int16 {
	data := r.take(2)
	if r.err != nil {
		return 0
	}
	return int16(binary.BigEndian.Uint16(data))
}

func (r *reader) int32() int32 {
	data := r.take(4)
	if r.err != nil {
		return 0
	}
	return int32(binary.BigEndian.Uint32(data))
}

func (r *reader) int64() int64 {
	data := r.take(8)
	if r.err != nil {
		return 0
	}
	return int64(binary.BigEndian.Uint64(data))
}

func (r *reader) bool() bool {
	data := r.take(1)
	if r.err != nil {
		return false
	}
	return data[0] != 0
}

// string 解码非空 STRING；NULLABLE_STRING 的 -1 长度返回空串。
func (r *reader) string() string {
	length := r.int16()
	if r.err != nil || length < 0 {
		return ""
	}
	return string(r.take(int(length)))
}

// arrayLen 读取数组长度并做上界防御；null array(-1) 归一为 0。
func (r *reader) arrayLen() int {
	length := r.int32()
	if r.err != nil {
		return 0
	}
	if length < 0 {
		return 0
	}
	if int(length) > maxResponseBytes {
		r.err = fmt.Errorf("kafka array length %d exceeds limit", length)
		return 0
	}
	return int(length)
}

func (r *reader) int32Array() []int32 {
	count := r.arrayLen()
	values := make([]int32, 0, count)
	for range count {
		values = append(values, r.int32())
	}
	return values
}

// conn 复用一条 TCP 连接顺序执行请求，correlation id 自增。
type conn struct {
	transport   net.Conn
	correlation int32
}

// roundTrip 发送一个请求帧并读回响应体（已剥掉 size 与 correlation id）。
func (c *conn) roundTrip(apiKey, apiVersion int16, body []byte) (*reader, error) {
	c.correlation++
	header := &writer{}
	header.int16(apiKey)
	header.int16(apiVersion)
	header.int32(c.correlation)
	header.string(clientID)

	frame := &writer{}
	frame.int32(int32(len(header.buffer) + len(body)))
	frame.buffer = append(frame.buffer, header.buffer...)
	frame.buffer = append(frame.buffer, body...)
	if _, err := c.transport.Write(frame.buffer); err != nil {
		return nil, fmt.Errorf("write kafka request api %d: %w", apiKey, err)
	}

	sizeBytes := make([]byte, 4)
	if _, err := io.ReadFull(c.transport, sizeBytes); err != nil {
		return nil, fmt.Errorf("read kafka response size api %d: %w", apiKey, err)
	}
	size := int32(binary.BigEndian.Uint32(sizeBytes))
	if size < 4 || size > maxResponseBytes {
		return nil, fmt.Errorf("kafka response size %d out of range", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.transport, payload); err != nil {
		return nil, fmt.Errorf("read kafka response api %d: %w", apiKey, err)
	}
	response := &reader{buffer: payload}
	if got := response.int32(); got != c.correlation {
		return nil, fmt.Errorf("kafka correlation id %d, want %d", got, c.correlation)
	}
	return response, nil
}

// apiVersions 用 ApiVersions v0 确认对端说 Kafka 协议且 broker 可服务。
func (c *conn) apiVersions() error {
	response, err := c.roundTrip(apiKeyAPIVersions, 0, nil)
	if err != nil {
		return err
	}
	errorCode := response.int16()
	count := response.arrayLen()
	for range count {
		response.int16()
		response.int16()
		response.int16()
	}
	if response.err != nil {
		return response.err
	}
	if errorCode != 0 {
		return fmt.Errorf("kafka ApiVersions error code %d", errorCode)
	}
	return nil
}

type topicMetadata struct {
	clusterID   string
	brokerCount int
	errorCode   int16
	partitions  []int32
}

// metadata 用 Metadata v2 取 cluster id、broker 数和目标 topic 的分区列表。
func (c *conn) metadata(topic string) (topicMetadata, error) {
	body := &writer{}
	body.int32(1)
	body.string(topic)
	response, err := c.roundTrip(apiKeyMetadata, 2, body.buffer)
	if err != nil {
		return topicMetadata{}, err
	}

	result := topicMetadata{}
	brokers := response.arrayLen()
	result.brokerCount = brokers
	for range brokers {
		response.int32()  // 节点 ID
		response.string() // 主机
		response.int32()  // 端口
		response.string() // rack（可空）
	}
	result.clusterID = response.string()
	response.int32() // controller ID
	topics := response.arrayLen()
	for range topics {
		errorCode := response.int16()
		name := response.string()
		response.bool() // 是否内部 topic
		partitions := response.arrayLen()
		ids := make([]int32, 0, partitions)
		for range partitions {
			response.int16() // 分区错误码
			ids = append(ids, response.int32())
			response.int32()      // leader
			response.int32Array() // 副本
			response.int32Array() // 同步副本
		}
		if name == topic {
			result.errorCode = errorCode
			result.partitions = ids
		}
	}
	if response.err != nil {
		return topicMetadata{}, response.err
	}
	return result, nil
}

// listGroups 用 ListGroups v0 列出 broker 上的消费组。
func (c *conn) listGroups() ([]string, error) {
	response, err := c.roundTrip(apiKeyListGroups, 0, nil)
	if err != nil {
		return nil, err
	}
	errorCode := response.int16()
	count := response.arrayLen()
	groups := make([]string, 0, count)
	for range count {
		groups = append(groups, response.string())
		response.string() // 协议类型
	}
	if response.err != nil {
		return nil, response.err
	}
	if errorCode != 0 {
		return nil, fmt.Errorf("kafka ListGroups error code %d", errorCode)
	}
	return groups, nil
}

// bytes 解码 NULLABLE_BYTES 并丢弃内容，只用于跳过成员元数据。
func (r *reader) skipBytes() {
	length := r.int32()
	if r.err != nil || length < 0 {
		return
	}
	r.take(int(length))
}

type groupState struct {
	state   string
	members int
}

// describeGroups 用 DescribeGroups v0 取消费组状态与成员数，
// 用于区分活跃组（Stable，有成员）和重启遗留的空组（Empty）。
func (c *conn) describeGroups(groups []string) (map[string]groupState, error) {
	body := &writer{}
	body.int32(int32(len(groups)))
	for _, group := range groups {
		body.string(group)
	}
	response, err := c.roundTrip(apiKeyDescribeGroups, 0, body.buffer)
	if err != nil {
		return nil, err
	}

	states := map[string]groupState{}
	count := response.arrayLen()
	for range count {
		errorCode := response.int16()
		group := response.string()
		state := response.string()
		response.string() // 协议类型
		response.string() // 协议
		members := response.arrayLen()
		for range members {
			response.string() // 成员 ID
			response.string() // 客户端 ID
			response.string() // 客户端主机
			response.skipBytes()
			response.skipBytes()
		}
		if errorCode == 0 {
			states[group] = groupState{state: state, members: members}
		}
	}
	if response.err != nil {
		return nil, response.err
	}
	return states, nil
}

// committedOffsets 用 OffsetFetch v1 取某消费组在各分区的已提交位点；-1 表示无提交。
func (c *conn) committedOffsets(group string, topic string, partitions []int32) (map[int32]int64, error) {
	body := &writer{}
	body.string(group)
	body.int32(1)
	body.string(topic)
	body.int32(int32(len(partitions)))
	for _, partition := range partitions {
		body.int32(partition)
	}
	response, err := c.roundTrip(apiKeyOffsetFetch, 1, body.buffer)
	if err != nil {
		return nil, err
	}

	offsets := map[int32]int64{}
	topics := response.arrayLen()
	for range topics {
		response.string() // topic
		count := response.arrayLen()
		for range count {
			partition := response.int32()
			offset := response.int64()
			response.string() // 元数据（可空）
			errorCode := response.int16()
			if errorCode == 0 {
				offsets[partition] = offset
			}
		}
	}
	if response.err != nil {
		return nil, response.err
	}
	return offsets, nil
}

// endOffsets 用 ListOffsets v1 (timestamp=-1) 取各分区的 log end offset。
func (c *conn) endOffsets(topic string, partitions []int32) (map[int32]int64, error) {
	body := &writer{}
	body.int32(-1) // 副本 ID
	body.int32(1)
	body.string(topic)
	body.int32(int32(len(partitions)))
	for _, partition := range partitions {
		body.int32(partition)
		body.int64(-1) // 最新位点
	}
	response, err := c.roundTrip(apiKeyListOffsets, 1, body.buffer)
	if err != nil {
		return nil, err
	}

	offsets := map[int32]int64{}
	topics := response.arrayLen()
	for range topics {
		response.string() // topic
		count := response.arrayLen()
		for range count {
			partition := response.int32()
			errorCode := response.int16()
			response.int64() // 时间戳
			offset := response.int64()
			if errorCode == 0 {
				offsets[partition] = offset
			}
		}
	}
	if response.err != nil {
		return nil, response.err
	}
	return offsets, nil
}
