// Package protocol defines the NATS message envelope shared by all agent
// communication as specified in docs/protocol.md.
package protocol

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

// Message type constants — used in Envelope.Type.
const (
	TypeCommand      = "command"
	TypeHeartbeat    = "heartbeat"
	TypeTelemetry    = "telemetry"
	TypeDiskHealth   = "disk_health"
	TypeAlert        = "alert"
	TypeIPMI         = "ipmi"
	TypeResult       = "result"
	TypeCapabilities = "capabilities"
	TypeS3Telemetry  = "s3_telemetry"
	TypeS3Health     = "s3_health"
)

// Result status constants — used in ResultPayload.Status.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRejected  = "rejected"
)

// Envelope is the top-level wrapper for every NATS message in both directions.
type Envelope struct {
	V         int             `json:"v"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	AgentType string          `json:"agent_type"`
	AgentUUID string          `json:"agent_uuid"`
	Timestamp int64           `json:"timestamp"`
	// ReplyTo is set by the platform on sync commands (AgentCommandService::send).
	// The agent publishes the result directly to this inbox subject instead of
	// the evt subject. Absent on all other messages.
	ReplyTo   string          `json:"reply_to,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// CommandPayload is the payload shape for platform → agent command messages.
type CommandPayload struct {
	Operation string          `json:"operation"`
	Params    json.RawMessage `json:"params,omitempty"`
	TimeoutS  int             `json:"timeout_s,omitempty"`
}

// ResultPayload is the payload shape for agent → platform result messages.
type ResultPayload struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	Output    any    `json:"output,omitempty"`
}

// HeartbeatPayload is the payload shape for heartbeat events.
type HeartbeatPayload struct {
	Version     string `json:"version"`
	UptimeS     int64  `json:"uptime_s"`
	TasksQueued int    `json:"tasks_queued"`
}

// OperationParam describes a single parameter accepted by an operation.
type OperationParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`                  // "string", "integer", "array"
	Required    bool   `json:"required"`
	Description string `json:"description"`
	MinValue    *int   `json:"min_value,omitempty"`   // for integer types
}

// OperationSchema describes one executable operation including its parameters.
type OperationSchema struct {
	Operation   string           `json:"operation"`
	Description string           `json:"description"`
	Params      []OperationParam `json:"params,omitempty"`
}

// CapabilitiesPayload is published once on agent startup so the platform knows
// which operations are currently enabled and what parameters each one accepts.
// ExecCommands is only present when the "exec" operation is enabled.
type CapabilitiesPayload struct {
	Operations   []OperationSchema `json:"operations"`
	ExecCommands []string          `json:"exec_commands,omitempty"`
}

// New returns a new Envelope with a generated UUID v4 ID and current timestamp.
func New(agentUUID, agentType, msgType string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshalling payload: %w", err)
	}
	return Envelope{
		V:         1,
		ID:        newUUID(),
		Type:      msgType,
		AgentType: agentType,
		AgentUUID: agentUUID,
		Timestamp: time.Now().Unix(),
		Payload:   raw,
	}, nil
}

// DecodePayload unmarshals the envelope's Payload into dst.
func (e *Envelope) DecodePayload(dst any) error {
	return json.Unmarshal(e.Payload, dst)
}

// ResultFor constructs a result Envelope that echoes back the command ID from src.
// agentType should match the agent's configured type (e.g. "storage").
func ResultFor(src Envelope, agentUUID, agentType, status, message string, output any) (Envelope, error) {
	return New(agentUUID, agentType, TypeResult, ResultPayload{
		CommandID: src.ID,
		Status:    status,
		Message:   message,
		Output:    output,
	})
}

// newUUID generates a random UUID v4 string.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:]) //nolint:errcheck — crypto/rand.Read never errors on Linux
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
