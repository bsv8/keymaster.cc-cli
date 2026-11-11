package connect

import (
	"sync"
)

// Manager 持有授权、审批队列与当前运行代际。
// 代际取自后台实例标识；后台重启后运行代际变化，旧授权必须重新登录。
type Manager struct {
	auth       *AuthStore
	queue      *Queue
	payments   *PaymentStore
	mu         sync.Mutex
	generation string
}

func NewManager(walletDir string) *Manager {
	return &Manager{auth: NewAuthStore(walletDir), queue: NewQueue(), payments: NewPaymentStore(walletDir)}
}

func (m *Manager) AuthStore() *AuthStore { return m.auth }
func (m *Manager) Queue() *Queue         { return m.queue }
func (m *Manager) Payments() *PaymentStore { return m.payments }

func (m *Manager) Load() error {
	if err := m.auth.Load(); err != nil {
		return err
	}
	if err := m.payments.Load(); err != nil {
		return err
	}
	// 重启收敛：审批与 held 计划只在内存，遗留 pending 不能永久悬挂。
	return m.payments.ReconcileAfterRestart()
}

// SetGeneration 设置当前运行代际（后台实例标识），每次启动后调用一次。
func (m *Manager) SetGeneration(g string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation = g
}

// Generation 返回当前运行代际。
func (m *Manager) Generation() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}
