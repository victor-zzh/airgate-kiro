package gateway

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

const (
	preludeLen       = 12 // totalLen(4) + headerLen(4) + preludeCRC(4)
	messageCRCLen    = 4
	minMessageLen    = 16 // prelude(12) + messageCRC(4)
	maxMessageLen    = 16 * 1024 * 1024
	maxConsecErrors  = 5

	headerTypeString = 7
)

var (
	ErrTooManyErrors = errors.New("eventstream: too many consecutive decode errors")
	ErrMessageTooBig = errors.New("eventstream: message exceeds 16MB limit")
	ErrPreludeCRC    = errors.New("eventstream: prelude CRC mismatch")
	ErrMessageCRC    = errors.New("eventstream: message CRC mismatch")
)

// KiroEvent 从 AWS Event Stream 帧中解析出的事件。
type KiroEvent struct {
	MessageType string // "event", "error", "exception"
	EventType   string // "assistantResponseEvent", "toolUseEvent", "contextUsageEvent", "meteringEvent"
	Payload     []byte
	ErrorCode   string
	ErrorMsg    string
}

// EventStreamDecoder 从 io.Reader 解码 AWS Event Stream 二进制帧。
type EventStreamDecoder struct {
	reader   io.Reader
	errCount int
	buf      []byte
}

// NewEventStreamDecoder 创建解码器。
func NewEventStreamDecoder(r io.Reader) *EventStreamDecoder {
	return &EventStreamDecoder{
		reader: r,
		buf:    make([]byte, 0, 4096),
	}
}

// Next 读取下一个事件。EOF 表示流结束。
func (d *EventStreamDecoder) Next() (*KiroEvent, error) {
	for {
		if d.errCount >= maxConsecErrors {
			return nil, ErrTooManyErrors
		}

		event, err := d.decodeFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			d.errCount++
			continue
		}

		d.errCount = 0
		return event, nil
	}
}

func (d *EventStreamDecoder) decodeFrame() (*KiroEvent, error) {
	var prelude [preludeLen]byte
	if _, err := io.ReadFull(d.reader, prelude[:]); err != nil {
		return nil, err
	}

	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headerLen := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	if totalLen < minMessageLen || totalLen > maxMessageLen {
		return nil, ErrMessageTooBig
	}

	computed := crc32.ChecksumIEEE(prelude[0:8])
	if computed != preludeCRC {
		return nil, ErrPreludeCRC
	}

	remainLen := int(totalLen) - preludeLen
	if cap(d.buf) < remainLen {
		d.buf = make([]byte, remainLen)
	} else {
		d.buf = d.buf[:remainLen]
	}
	if _, err := io.ReadFull(d.reader, d.buf); err != nil {
		return nil, err
	}

	fullMsg := make([]byte, int(totalLen))
	copy(fullMsg, prelude[:])
	copy(fullMsg[preludeLen:], d.buf)

	msgCRCOffset := len(fullMsg) - messageCRCLen
	expectedCRC := binary.BigEndian.Uint32(fullMsg[msgCRCOffset:])
	actualCRC := crc32.ChecksumIEEE(fullMsg[:msgCRCOffset])
	if expectedCRC != actualCRC {
		return nil, ErrMessageCRC
	}

	headerData := d.buf[:headerLen]
	payloadEnd := remainLen - messageCRCLen
	var payload []byte
	if int(headerLen) < payloadEnd {
		payload = d.buf[headerLen:payloadEnd]
	}

	headers, err := parseHeaders(headerData)
	if err != nil {
		return nil, fmt.Errorf("eventstream: header parse: %w", err)
	}

	event := &KiroEvent{
		MessageType: headers[":message-type"],
		EventType:   headers[":event-type"],
		Payload:     payload,
	}

	switch event.MessageType {
	case "error":
		event.ErrorCode = headers[":error-code"]
		event.ErrorMsg = string(payload)
	case "exception":
		event.EventType = headers[":exception-type"]
		event.ErrorMsg = string(payload)
	}

	return event, nil
}

func parseHeaders(data []byte) (map[string]string, error) {
	headers := make(map[string]string)
	pos := 0

	for pos < len(data) {
		if pos >= len(data) {
			break
		}
		nameLen := int(data[pos])
		pos++
		if pos+nameLen > len(data) {
			return headers, fmt.Errorf("header name overflow at %d", pos)
		}
		name := string(data[pos : pos+nameLen])
		pos += nameLen

		if pos >= len(data) {
			return headers, fmt.Errorf("missing header type at %d", pos)
		}
		typeID := data[pos]
		pos++

		switch typeID {
		case 0: // BoolTrue
			headers[name] = "true"
		case 1: // BoolFalse
			headers[name] = "false"
		case 2: // Byte
			pos++
		case 3: // Short
			pos += 2
		case 4: // Integer
			pos += 4
		case 5: // Long
			pos += 8
		case 6: // ByteArray
			if pos+2 > len(data) {
				return headers, fmt.Errorf("byte array len overflow at %d", pos)
			}
			vLen := int(binary.BigEndian.Uint16(data[pos:]))
			pos += 2 + vLen
		case headerTypeString: // String
			if pos+2 > len(data) {
				return headers, fmt.Errorf("string len overflow at %d", pos)
			}
			vLen := int(binary.BigEndian.Uint16(data[pos:]))
			pos += 2
			if pos+vLen > len(data) {
				return headers, fmt.Errorf("string value overflow at %d", pos)
			}
			headers[name] = string(data[pos : pos+vLen])
			pos += vLen
		case 8: // Timestamp
			pos += 8
		case 9: // UUID
			pos += 16
		default:
			return headers, fmt.Errorf("unknown header type %d at %d", typeID, pos)
		}
	}

	return headers, nil
}

// ParseAssistantResponsePayload 解析 assistantResponseEvent 的 content 字段。
func ParseAssistantResponsePayload(payload []byte) string {
	var p struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Content
}

// ToolUsePayload toolUseEvent 的 payload。
type ToolUsePayload struct {
	Name      string `json:"name"`
	ToolUseID string `json:"toolUseId"`
	Input     string `json:"input"`
	Stop      bool   `json:"stop"`
}

// ParseToolUsePayload 解析 toolUseEvent。
func ParseToolUsePayload(payload []byte) (*ToolUsePayload, error) {
	var p ToolUsePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ContextUsagePayload contextUsageEvent 的 payload。
type ContextUsagePayload struct {
	ContextUsagePercentage float64 `json:"contextUsagePercentage"`
}

// ParseContextUsagePayload 解析 contextUsageEvent。
func ParseContextUsagePayload(payload []byte) (*ContextUsagePayload, error) {
	var p ContextUsagePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
