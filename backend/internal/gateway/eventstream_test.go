package gateway

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"testing"
)

// buildEventStreamFrame 构造一个合法的 AWS Event Stream 帧。
func buildEventStreamFrame(headers map[string]string, payload []byte) []byte {
	// 编码 headers
	var headerBuf bytes.Buffer
	for name, value := range headers {
		headerBuf.WriteByte(byte(len(name)))
		headerBuf.WriteString(name)
		headerBuf.WriteByte(headerTypeString) // type 7 = String
		binary.Write(&headerBuf, binary.BigEndian, uint16(len(value)))
		headerBuf.WriteString(value)
	}
	headerData := headerBuf.Bytes()

	totalLen := uint32(preludeLen + len(headerData) + len(payload) + messageCRCLen)
	headerLen := uint32(len(headerData))

	// Prelude: totalLen + headerLen
	var prelude [8]byte
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], headerLen)
	preludeCRC := crc32.ChecksumIEEE(prelude[:])

	var msg bytes.Buffer
	msg.Write(prelude[:])
	binary.Write(&msg, binary.BigEndian, preludeCRC)
	msg.Write(headerData)
	msg.Write(payload)

	msgCRC := crc32.ChecksumIEEE(msg.Bytes())
	binary.Write(&msg, binary.BigEndian, msgCRC)

	return msg.Bytes()
}

func TestEventStreamDecoder_BasicEvent(t *testing.T) {
	frame := buildEventStreamFrame(
		map[string]string{
			":message-type": "event",
			":event-type":   "assistantResponseEvent",
		},
		[]byte(`{"content":"Hello"}`),
	)

	decoder := NewEventStreamDecoder(bytes.NewReader(frame))
	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if event.MessageType != "event" {
		t.Errorf("expected message-type 'event', got %q", event.MessageType)
	}
	if event.EventType != "assistantResponseEvent" {
		t.Errorf("expected event-type 'assistantResponseEvent', got %q", event.EventType)
	}

	content := ParseAssistantResponsePayload(event.Payload)
	if content != "Hello" {
		t.Errorf("expected content 'Hello', got %q", content)
	}
}

func TestEventStreamDecoder_MultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildEventStreamFrame(
		map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"},
		[]byte(`{"content":"Hello "}`),
	))
	buf.Write(buildEventStreamFrame(
		map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"},
		[]byte(`{"content":"World"}`),
	))

	decoder := NewEventStreamDecoder(&buf)

	event1, err := decoder.Next()
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if ParseAssistantResponsePayload(event1.Payload) != "Hello " {
		t.Error("frame 1 content mismatch")
	}

	event2, err := decoder.Next()
	if err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if ParseAssistantResponsePayload(event2.Payload) != "World" {
		t.Error("frame 2 content mismatch")
	}

	_, err = decoder.Next()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestEventStreamDecoder_ToolUseEvent(t *testing.T) {
	frame := buildEventStreamFrame(
		map[string]string{":message-type": "event", ":event-type": "toolUseEvent"},
		[]byte(`{"name":"read_file","toolUseId":"toolu_123","input":"{\"path\":\"/tmp\"}","stop":true}`),
	)

	decoder := NewEventStreamDecoder(bytes.NewReader(frame))
	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tu, err := ParseToolUsePayload(event.Payload)
	if err != nil {
		t.Fatalf("parse tool use: %v", err)
	}
	if tu.Name != "read_file" {
		t.Errorf("expected name 'read_file', got %q", tu.Name)
	}
	if !tu.Stop {
		t.Error("expected stop=true")
	}
}

func TestEventStreamDecoder_ContextUsageEvent(t *testing.T) {
	frame := buildEventStreamFrame(
		map[string]string{":message-type": "event", ":event-type": "contextUsageEvent"},
		[]byte(`{"contextUsagePercentage":42.5}`),
	)

	decoder := NewEventStreamDecoder(bytes.NewReader(frame))
	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cu, err := ParseContextUsagePayload(event.Payload)
	if err != nil {
		t.Fatalf("parse context usage: %v", err)
	}
	if cu.ContextUsagePercentage != 42.5 {
		t.Errorf("expected 42.5, got %f", cu.ContextUsagePercentage)
	}
}

func TestEventStreamDecoder_ErrorEvent(t *testing.T) {
	frame := buildEventStreamFrame(
		map[string]string{":message-type": "error", ":error-code": "Throttling"},
		[]byte("Rate exceeded"),
	)

	decoder := NewEventStreamDecoder(bytes.NewReader(frame))
	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.MessageType != "error" {
		t.Errorf("expected 'error', got %q", event.MessageType)
	}
	if event.ErrorCode != "Throttling" {
		t.Errorf("expected error code 'Throttling', got %q", event.ErrorCode)
	}
}

func TestEventStreamDecoder_CRCFailure(t *testing.T) {
	frame := buildEventStreamFrame(
		map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"},
		[]byte(`{"content":"test"}`),
	)
	// 篡改 payload 使 CRC 失败
	frame[len(frame)-5] ^= 0xFF

	decoder := NewEventStreamDecoder(bytes.NewReader(frame))
	_, err := decoder.Next()
	if err != io.EOF {
		t.Errorf("expected EOF after CRC failure, got %v", err)
	}
}

func TestEventStreamDecoder_EmptyPayload(t *testing.T) {
	frame := buildEventStreamFrame(
		map[string]string{":message-type": "event", ":event-type": "meteringEvent"},
		nil,
	)

	decoder := NewEventStreamDecoder(bytes.NewReader(frame))
	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.EventType != "meteringEvent" {
		t.Errorf("expected 'meteringEvent', got %q", event.EventType)
	}
}
