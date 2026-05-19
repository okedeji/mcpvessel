package cage

import "time"

type Type int

const (
	TypeUnspecified Type = iota
	TypeDiscovery
	TypeValidator
	TypeExploitation
)

func (t Type) String() string {
	switch t {
	case TypeDiscovery:
		return "discovery"
	case TypeValidator:
		return "validator"
	case TypeExploitation:
		return "exploitation"
	default:
		return "unspecified"
	}
}

func TypeFromString(s string) Type {
	switch s {
	case "discovery":
		return TypeDiscovery
	case "validator":
		return TypeValidator
	case "exploitation":
		return TypeExploitation
	default:
		return TypeUnspecified
	}
}

type State int

const (
	StatePending State = iota
	StateProvisioning
	StateRunning
	StatePaused
	StateTearingDown
	StateCompleted
	StateFailed
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateProvisioning:
		return "provisioning"
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateTearingDown:
		return "tearing_down"
	case StateCompleted:
		return "completed"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

func StateFromString(s string) State {
	switch s {
	case "pending":
		return StatePending
	case "provisioning":
		return StateProvisioning
	case "running":
		return StateRunning
	case "paused":
		return StatePaused
	case "tearing_down":
		return StateTearingDown
	case "completed":
		return StateCompleted
	case "failed":
		return StateFailed
	default:
		return StatePending
	}
}

type Config struct {
	AssessmentID    string
	Type            Type
	BundleRef       string
	Scope           Scope
	Resources       ResourceLimits
	TimeLimits      TimeLimits
	RateLimits      RateLimits
	LLM             *LLMGatewayConfig
	ProxyConfig     ProxyConfig
	ParentFindingID string
	VulnClass       string
	SkipPaths       []string
	Guidance        []byte
	InputContext    []byte
	Credentials     string
	ProofThreshold  float64
	Environment     map[string]string
}

type Scope struct {
	Host   string
	Ports  []string
	Paths  []string
	Extras []string
}

type ResourceLimits struct {
	VCPUs    int32
	MemoryMB int32
}

type TimeLimits struct {
	MaxDuration time.Duration
}

type RateLimits struct {
	RequestsPerSecond int32
}

type LLMGatewayConfig struct {
	TokenBudget     int64
	RoutingStrategy string
}

type ProxyConfig struct {
	JudgeEndpoint   string
	JudgeConfidence float64
	JudgeTimeoutSec int
}

type Info struct {
	ID           string
	AssessmentID string
	Type         Type
	State        State
	Error        string
	Config       Config
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
