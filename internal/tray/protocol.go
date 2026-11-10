package tray

// Message 是父子进程之间的一帧。NDJSON 编码，一行一个 JSON 对象，
// UTF-8。单帧上限见 MaxFrameBytes；超限的帧被丢弃并记错，不破坏后续
// 帧的边界。
type Message struct {
	Type string `json:"type"`

	// status (parent -> child)
	State    string `json:"state,omitempty"`
	Locked   bool   `json:"locked,omitempty"`
	Network  string `json:"network,omitempty"`
	APIAddr  string `json:"apiAddr,omitempty"`
	WebAdmin bool   `json:"webAdmin,omitempty"`
	Detail   string `json:"detail,omitempty"`

	// action (child -> parent)
	Action string `json:"action,omitempty"`

	// error (either direction, transport-level only, never business data)
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Message types on the wire.
const (
	MsgStatus   = "status"
	MsgAction   = "action"
	MsgShutdown = "shutdown"
	MsgReady    = "ready"
	MsgPassword = "password"
	MsgError    = "error"
)

// Actions from child to parent.
const (
	ActionOpenAdmin = "open-admin"
	ActionRestart   = "restart"
	ActionQuit      = "quit"
)

// Error codes (transport-level only).
const (
	ErrFrameTooLarge = "frame-too-large"
	ErrBadFrame      = "bad-frame"
	ErrUnknownType   = "unknown-type"
	ErrNoTray        = "no-tray"
)

// MaxFrameBytes bounds a single NDJSON frame.
const MaxFrameBytes = 1 << 20
