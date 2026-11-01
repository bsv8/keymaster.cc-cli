package sell

import (
	bitfsdebug "github.com/bsv8/BitFSProtocol/debug"
	"github.com/bsv8/BitFSProtocol/wire"
	"time"
)

// debugSession 只观察真实传输，不替换协议、不复制保存 payload。
type debugSession struct {
	Session
	sessionID string
}

func (s *debugSession) Read() (wire.Artifact, error) {
	started := time.Now()
	artifact, err := s.Session.Read()
	bitfsdebug.Log("keymaster.cc-cli", "transport.receive", func() any {
		return map[string]any{"sessionId": s.sessionID, "kind": artifact.Kind(), "bytes": len(artifact.Bytes()), "elapsedMs": float64(time.Since(started).Microseconds()) / 1000, "error": err}
	})
	return artifact, err
}
func (s *debugSession) Write(artifact wire.Artifact) error {
	started := time.Now()
	err := s.Session.Write(artifact)
	bitfsdebug.Log("keymaster.cc-cli", "transport.send", func() any {
		return map[string]any{"sessionId": s.sessionID, "kind": artifact.Kind(), "bytes": len(artifact.Bytes()), "elapsedMs": float64(time.Since(started).Microseconds()) / 1000, "error": err}
	})
	return err
}
