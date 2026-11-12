package connect

import (
	"sync"
)

// Manager 持有授权、审批队列与当前运行代际。
// 代际取自后台实例标识；后台重启后运行代际变化，旧授权必须重新登录。
// 许可与历史持久保存：许可按精确应用身份隔离，历史与许可变更原子提交。
type Manager struct {
	auth    *AuthStore
	queue   *Queue
	payments *PaymentStore
	launch  *LaunchStore
	apps    *AppState
	conns   *ConnectionStore
	mu      sync.Mutex
	// authMu 串行化“授权有效性校验＋许可提交”与“授权撤销/替换”：
	// 锁顺序统一为 authMu → queue → {authstore, appstate}，任何路径不得逆序持有。
	authMu     sync.Mutex
	generation string
}

// SerializedAuthChange 在授权生命周期锁内执行 fn；调用方进入时不得持有
// queue/appstate/authstore/daemon 锁（统一由外向内加锁，防止死锁）。
func (m *Manager) SerializedAuthChange(fn func()) {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	fn()
}

func NewManager(walletDir string) *Manager {
	return &Manager{auth: NewAuthStore(walletDir), queue: NewQueue(), payments: NewPaymentStore(walletDir), launch: NewLaunchStore(), apps: NewAppState(walletDir), conns: NewConnectionStore()}
}

func (m *Manager) AuthStore() *AuthStore { return m.auth }
func (m *Manager) Queue() *Queue         { return m.queue }
func (m *Manager) Payments() *PaymentStore { return m.payments }
func (m *Manager) LaunchStore() *LaunchStore { return m.launch }
// Permissions 与 History 由同一 AppState 背书：许可变更与对应历史事件在同一
// 锁内一次落盘，不用两次写盘加补偿代替事务。
func (m *Manager) Permissions() *AppState { return m.apps }
func (m *Manager) History() *AppState { return m.apps }
func (m *Manager) AppState() *AppState { return m.apps }
func (m *Manager) Connections() *ConnectionStore { return m.conns }

func (m *Manager) Load() error {
	if err := m.auth.Load(); err != nil {
		return err
	}
	if err := m.payments.Load(); err != nil {
		return err
	}
	if err := m.apps.Load(); err != nil {
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
