package cage

import "time"

type Type int

const (
	TypeUnspecified Type = iota
	TypeDiscovery
	TypeValidation
	TypeExploitation
)

func (t Type) String() string {
	switch t {
	case TypeDiscovery:
		return "discovery"
	case TypeValidation:
		return "validation"
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
	case "validation":
		return TypeValidation
	case "exploitation":
		return TypeExploitation
	default:
		return TypeUnspecified
	}
}

type State int

// Each state corresponds to an observable phase of the cage workflow.
// The workflow transitions explicitly via UpdateCageState at every
// checkpoint so the operator-facing display reflects what the cage is
// actually doing (e.g. "queued waiting for a fleet slot" vs "actively
// running its agent"). Without these transitions every cage would
// linger at Pending until completion, which is the bug the explicit
// state-machine wiring solves.
const (
	// StatePending: workflow has started, doing pre-slot prep
	// (ValidateCageConfig, IssueIdentity, FetchSecrets). Should be
	// brief — anything stuck here for >5s indicates a worker problem.
	StatePending State = iota
	// StateQueued: blocked at AcquireCageSlot waiting for fleet
	// capacity. Can be long on a busy host. Stuck here >5s indicates
	// the fleet is under-provisioned for the current demand.
	StateQueued
	// StateProvisioning: slot held. Assembling rootfs, booting
	// Firecracker, applying network policy. Heavy work, ~30s to a
	// few minutes.
	StateProvisioning
	// StateRunning: VM is up and MonitorCage is following the agent.
	// The bulk of the cage's lifetime is spent here.
	StateRunning
	// StatePaused: agent suspended by an intervention or directive.
	// Stays here until the operator resolves the intervention.
	StatePaused
	// StateTearingDown: MonitorCage exited; cleanup activities are
	// running (TeardownVM, RevokeSVID, RemoveNetworkPolicy, etc.).
	StateTearingDown
	// StateCompleted: all teardown succeeded and the agent exited 0.
	StateCompleted
	// StateFailed: any failure along the path. Includes teardown
	// failures because leaked SVIDs/Vault tokens/policies are
	// security-relevant in agentcage — they need operator visibility,
	// not a "completed-with-warnings" wallpaper.
	StateFailed
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateQueued:
		return "queued"
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
	case "queued":
		return StateQueued
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
	CustomerID      string
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
	CredentialsKey string
	ProofThreshold float64
	// IdentifyInRequests causes the payload proxy to inject an
	// X-Agentcage-Pentest header attributing traffic to this
	// assessment. Toggled at the assessment level; propagated to every
	// cage it spawns.
	IdentifyInRequests bool
	Environment        map[string]string
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
	JudgeEndpoint              string
	JudgeConfidence            float64
	JudgeTimeoutSec            int
	RequireJudgeForAllOutbound bool
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
