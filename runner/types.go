// Package runner owns the controller side of the experimental remote execution
// protocol. An agent thread remains on the controller; one attempt leases one
// remote sandbox for all of its environment tools.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/toolenv"
)

const ProtocolVersion = 1

var (
	ErrStoreConflict       = errors.New("runner state revision conflict")
	ErrThreadBusy          = errors.New("agent thread already has an active attempt")
	ErrSessionsActive      = errors.New("runner has reserved sessions; wait for cleanup or lease expiry before disabling")
	ErrFenced              = errors.New("remote attempt is fenced")
	ErrClosed              = errors.New("runner broker is closed")
	ErrUnauthorized        = errors.New("runner authentication failed")
	ErrUnknownOutcome      = errors.New("remote execution outcome is unknown; do not automatically replay")
	ErrCallConflict        = errors.New("tool call ID was reused with different input")
	ErrCapacityUnavailable = errors.New("runner_capacity_unavailable")
)

// StateStore must atomically compare and replace its revision. Only one broker
// owns a revision; this prevents a superseded controller from renewing leases.
// A missing record is Snapshot{}; Save(expected=0) creates it atomically.
type StateStore interface {
	Load(context.Context) (Snapshot, error)
	Save(context.Context, uint64, json.RawMessage) (uint64, error)
}

// DiagnosticStore commits a small content-free projection alongside the opaque
// checkpoint. Operators never need to expand tool transcripts to inspect leases.
type DiagnosticStore interface {
	SaveDiagnostic(context.Context, uint64, json.RawMessage, []SessionStatus) (uint64, error)
}

// StateGuard serializes a short artifact commit against a controller takeover.
// Persistent stores should implement this in the same transactional lock domain
// as Save. Without it, file commit fencing is limited to this Hub instance.
type StateGuard interface {
	Guard(context.Context, uint64, func() error) error
}

type Snapshot struct {
	Revision uint64
	Data     json.RawMessage
}

type Options struct {
	Store           StateStore
	LeaseDuration   time.Duration    // default 30 seconds
	RegistrationTTL time.Duration    // default 10 minutes, maximum one hour
	PollWait        time.Duration    // default 10 seconds; less than lease duration
	Clock           func() time.Time // injectable for deterministic lease tests
}

type GPU struct {
	ID        string `json:"id"`
	Model     string `json:"model,omitempty"`
	MemoryMiB int64  `json:"memory_mib,omitempty"`
}

type Resources struct {
	CPUMillis    int64  `json:"cpu_millis,omitempty"`
	MemoryMiB    int64  `json:"memory_mib,omitempty"`
	PIDs         int64  `json:"pids,omitempty"`
	GPUCount     int    `json:"gpu_count,omitempty"`
	GPUModel     string `json:"gpu_model,omitempty"`
	GPUMemoryMiB int64  `json:"gpu_memory_mib,omitempty"` // minimum advertised memory per device
}

type Capabilities struct {
	Executor     string            `json:"executor,omitempty"`
	Slots        int               `json:"slots"`
	Labels       map[string]string `json:"labels,omitempty"`
	Environments []string          `json:"environments,omitempty"`
	CPUMillis    int64             `json:"cpu_millis,omitempty"`
	MemoryMiB    int64             `json:"memory_mib,omitempty"`
	GPUs         []GPU             `json:"gpus,omitempty"` // whole-device exclusive reservations
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema map[string]any  `json:"input_schema,omitempty"`
	Kind        string          `json:"kind,omitempty"` // builtin, command, script, http, mcp
	Exec        json.RawMessage `json:"exec,omitempty"`
	ReadOnly    bool            `json:"read_only,omitempty"`
	Concurrent  bool            `json:"concurrent,omitempty"`
}

type MCPConfig struct {
	ID         int64             `json:"id,omitempty"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"` // stdio, http, sse
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Insecure   bool              `json:"insecure,omitempty"`
}

type RunSpec struct {
	PoolCPUMillis    int64             `json:"pool_cpu_millis,omitempty"`
	PoolMemoryMiB    int64             `json:"pool_memory_mib,omitempty"`
	ConcurrencyLimit int               `json:"concurrency_limit,omitempty"` // set by the broker, never by the agent
	NodeID           string            `json:"node_id,omitempty"`
	ThreadID         string            `json:"thread_id"`
	TaskID           string            `json:"task_id,omitempty"`
	AgentKey         string            `json:"agent_key,omitempty"`
	Environment      string            `json:"environment,omitempty"`
	Image            string            `json:"image,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	Resources        Resources         `json:"resources,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	WorkingDir       string            `json:"working_dir"`
	MaxOutputChars   int               `json:"max_output_chars,omitempty"`
	Tools            []ToolDefinition  `json:"tools"` // allowlist; MCPs additionally allow their discovered schemas
	MCPs             []MCPConfig       `json:"mcps,omitempty"`
	Files            json.RawMessage   `json:"files,omitempty"` // versioned runnerfiles manifest
	ConfigVersion    string            `json:"config_version,omitempty"`
}

type RegisterRequest struct {
	Token        string       `json:"token"`
	Name         string       `json:"name"`
	BootID       string       `json:"boot_id"`
	Capabilities Capabilities `json:"capabilities"`
	Protocol     int          `json:"protocol"`
}

type RegisterResponse struct {
	NodeID       string `json:"node_id"`
	Credential   string `json:"credential"` // shown once; controller persists only its hash
	Epoch        string `json:"epoch"`
	Protocol     int    `json:"protocol"`
	LeaseSeconds int    `json:"lease_seconds"`
}

type PollRequest struct {
	PoolStatus    *PoolStatus            `json:"pool_status,omitempty"`
	NodeID        string                 `json:"node_id"`
	BootID        string                 `json:"boot_id"`
	Protocol      int                    `json:"protocol"`
	Capabilities  *Capabilities          `json:"capabilities,omitempty"` // adopted on a new boot; same-boot changes require restart
	Notifications []NotificationEnvelope `json:"notifications,omitempty"`
}

type NotificationEnvelope struct {
	ID        string              `json:"id"`
	SessionID string              `json:"session_id"`
	AttemptID string              `json:"attempt_id"`
	Items     []tool.Notification `json:"items"`
}

type Lease struct {
	SessionID string    `json:"session_id"`
	AttemptID string    `json:"attempt_id"`
	Epoch     string    `json:"epoch"`
	Until     time.Time `json:"until"`
}

type PollResponse struct {
	AdmissionDiagnostics      bool        `json:"admission_diagnostics,omitempty"`
	Heartbeat                 bool        `json:"heartbeat,omitempty"`
	Pool                      *PoolPolicy `json:"pool,omitempty"`
	Epoch                     string      `json:"epoch"`
	ServerTime                time.Time   `json:"server_time"`
	Leases                    []Lease     `json:"leases"`   // complete live set for this boot; absent/expired sessions must stop
	Commands                  []Command   `json:"commands"` // redelivered until acknowledged; node must deduplicate ID
	AcknowledgedNotifications []string    `json:"acknowledged_notifications,omitempty"`
}

// Heartbeats carry lifecycle metadata only, independently of command delivery.
// Nodes negotiate support through PollResponse before using this endpoint.
type HeartbeatRequest struct {
	PoolStatus *PoolStatus      `json:"pool_status,omitempty"`
	NodeID     string           `json:"node_id"`
	BootID     string           `json:"boot_id"`
	Protocol   int              `json:"protocol"`
	Stopped    []StoppedSession `json:"stopped,omitempty"`
}
type StoppedSession struct {
	SessionID string `json:"session_id"`
	AttemptID string `json:"attempt_id"`
	Epoch     string `json:"epoch"`
	Reason    string `json:"reason"`
}

const (
	CommandOpen  = "open"
	CommandCall  = "call"
	CommandClose = "close"
)

type Command struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	SessionID  string    `json:"session_id"`
	AttemptID  string    `json:"attempt_id"`
	Epoch      string    `json:"epoch"`
	LeaseUntil time.Time `json:"lease_until"`
	Spec       *RunSpec  `json:"spec,omitempty"`
	GPUIds     []string  `json:"gpu_ids,omitempty"`
	Call       *ToolCall `json:"call,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Abort      bool      `json:"abort,omitempty"` // true: stop now, no final file writes; false: flush then close
}

type ToolCall struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Input          json.RawMessage `json:"input"`
	WorkingDir     string          `json:"working_dir,omitempty"`
	OutputDir      string          `json:"output_dir,omitempty"`
	MaxOutputChars int             `json:"max_output_chars,omitempty"`
	Env            []string        `json:"env,omitempty"`
	Deadline       time.Time       `json:"deadline,omitempty"`
}

type ResultRequest struct {
	EnvironmentTools toolenv.Inventory   `json:"environment_tools,omitempty"`
	NodeID           string              `json:"node_id"`
	BootID           string              `json:"boot_id"`
	CommandID        string              `json:"command_id"`
	SessionID        string              `json:"session_id"`
	AttemptID        string              `json:"attempt_id"`
	Epoch            string              `json:"epoch"`
	Result           tool.Result         `json:"result"`
	MCPTools         []ToolDefinition    `json:"mcp_tools,omitempty"` // Open acknowledgment only
	Notifications    []tool.Notification `json:"notifications,omitempty"`
	Error            string              `json:"error,omitempty"` // execution infrastructure failure, not ordinary tool.IsError
	UnknownOutcome   bool                `json:"unknown_outcome,omitempty"`
}

type ResultResponse struct {
	Accepted bool `json:"accepted"`
}

type NodeView struct {
	PoolStatus         *PoolStatus  `json:"pool_status,omitempty"`
	EnvironmentPrompt  string       `json:"environment_prompt"`
	CPULimitMillis     int64        `json:"cpu_limit_millis"`
	MemoryLimitMiB     int64        `json:"memory_limit_mib"`
	EffectiveCPUMillis int64        `json:"effective_cpu_millis"`
	EffectiveMemoryMiB int64        `json:"effective_memory_mib"`
	ReservedCPUMillis  int64        `json:"reserved_cpu_millis"`
	ReservedMemoryMiB  int64        `json:"reserved_memory_mib"`
	ConcurrencyLimit   int          `json:"concurrency_limit"`
	EffectiveSlots     int          `json:"effective_slots"`
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	BootID             string       `json:"boot_id"`
	Enabled            bool         `json:"enabled"`
	Online             bool         `json:"online"`
	LastSeen           time.Time    `json:"last_seen"`
	Capabilities       Capabilities `json:"capabilities"`
	ActiveSessions     int          `json:"active_sessions"`
	ReservedGPUs       []string     `json:"reserved_gpus,omitempty"`
}

// Pool policy is supplied by the controller; status is observational only and
// never used to release reservations or grant execution authority.
type WarmProfile struct {
	Environment string            `json:"environment"`
	Idle        int               `json:"idle"`
	Image       string            `json:"image"`
	Resources   Resources         `json:"resources"`
	Env         map[string]string `json:"env,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	Files       json.RawMessage   `json:"files,omitempty"`
	NodeID      string            `json:"node_id,omitempty"`
}
type PoolPolicy struct {
	Slots     int           `json:"slots"`
	CPUMillis int64         `json:"cpu_millis"`
	MemoryMiB int64         `json:"memory_mib"`
	Profiles  []WarmProfile `json:"profiles"`
}
type PoolStatus struct {
	Admission *AdmissionStatus `json:"admission,omitempty"`
	Idle      int              `json:"idle"`
	Preparing int              `json:"preparing"`
	Pods      int              `json:"pods"`
	CPUMillis int64            `json:"cpu_millis"`
	MemoryMiB int64            `json:"memory_mib"`
	Error     string           `json:"error,omitempty"`
}

// Admission diagnostics contain only lifecycle IDs and numeric quota metadata.
// Quota values are a snapshot of the last rejection, not live namespace usage.
type AdmissionStatus struct {
	ObservedAt         time.Time       `json:"observed_at"`
	Waiting            []AdmissionWait `json:"waiting"`
	QuotaRejections    uint64          `json:"quota_rejections"`
	LastQuotaRejection *QuotaRejection `json:"last_quota_rejection,omitempty"`
	LastCreateAt       time.Time       `json:"last_create_at,omitempty"`
	RetryAt            time.Time       `json:"retry_at,omitempty"`
}
type AdmissionWait struct {
	SessionID string    `json:"session_id"`
	Stage     string    `json:"stage"`
	Since     time.Time `json:"since"`
	Attempts  int       `json:"attempts"`
}
type QuotaRejection struct {
	At        time.Time         `json:"at"`
	Name      string            `json:"name"`
	Requested map[string]string `json:"requested"`
	Used      map[string]string `json:"used"`
	Hard      map[string]string `json:"hard"`
}
