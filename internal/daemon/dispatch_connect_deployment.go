package daemon

import (
	"context"
	"fmt"
	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	"github.com/bsv8/keymaster.cc-cli/internal/appdeployment"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
	"os"
)

// downloadDeploymentFile 复用普通任务的调度、资金控制和事件，不增加下载协议。
func (d *Daemon) downloadDeploymentFile(ctx context.Context, seed string) ([]byte, error) {
	if _, _, err := d.ensureTask(ctx, seed); err != nil {
		return nil, err
	}
	updates := make(chan Task, 1)
	d.tasks.mu.Lock()
	task := d.tasks.tasks[seed]
	if task == nil {
		d.tasks.mu.Unlock()
		return nil, fmt.Errorf("download task missing")
	}
	if task.subs == nil {
		task.subs = map[chan Task]struct{}{}
	}
	task.subs[updates] = struct{}{}
	snapshot := *task
	done := task.done
	d.tasks.mu.Unlock()
	defer func() { d.tasks.mu.Lock(); delete(task.subs, updates); d.tasks.mu.Unlock() }()
	for {
		switch snapshot.State {
		case TaskDone:
			stat, err := os.Lstat(snapshot.FinalPath)
			if err != nil {
				return nil, err
			}
			if !stat.Mode().IsRegular() {
				return nil, fmt.Errorf("download result is not a regular file")
			}
			return os.ReadFile(snapshot.FinalPath)
		case TaskFailed, TaskCancelled:
			return nil, fmt.Errorf("download failed: %s", snapshot.Error)
		}
		select {
		case snapshot = <-updates:
		case <-done:
			d.tasks.mu.Lock()
			snapshot = *task
			d.tasks.mu.Unlock()
			done = nil
		case <-ctx.Done():
			return nil, fmt.Errorf("deployment download did not complete: %w", ctx.Err())
		}
	}
}
func (d *Daemon) invalidateDeployment(record appdeployment.Record) {
	if d.connectMgr == nil {
		return
	}
	d.connectMgr.LaunchStore().InvalidateDeployment(record.Origin, record.DeploymentID, record.Generation)
	for _, auth := range d.connectMgr.AuthStore().List() {
		if auth.DeploymentID != record.DeploymentID || auth.Origin != record.Origin || auth.DeploymentGeneration != record.Generation {
			continue
		}
		d.connectMgr.SerializedAuthChange(func() {
			_ = d.connectMgr.AuthStore().RevokeByID(auth.AuthorizationID)
			d.connectMgr.Connections().Close(auth.AuthorizationID)
		})
		for _, pending := range d.connectMgr.Queue().List() {
			if pending.AuthorizationID != auth.AuthorizationID {
				continue
			}
			if pending.Status == "pending" {
				_, _, _ = d.connectMgr.Queue().Decide(pending.ID, "cancel")
			} else if pending.Status == "approved" {
				_, _ = d.connectMgr.Queue().AbandonApproved(pending.ID)
			}
			if pending.Method == "payment.pay" {
				d.rejectPaymentByApproval(pending, "deployment-stopped")
			}
		}
	}
	d.publishConnect()
}
func (d *Daemon) deploymentAccess(ctx context.Context) *kmapi.ErrorDetail {
	if isAppCaller(ctx) {
		return errDetail(kmapi.ErrOriginRejected, "App cannot manage deployments")
	}
	if err := d.needInit(); err != nil {
		return err
	}
	if err := d.needRunning(); err != nil {
		return err
	}
	return d.needUnlocked()
}
func (d *Daemon) verifyDirectoryFilesDeployment(expectedAppPublicKeyHex, seed string) error {
	if d.connectMgr == nil || d.deployments == nil {
		return fmt.Errorf("Connect 未就绪")
	}
	record, err := d.deployments.RecordBySeed(seed)
	if err != nil {
		return fmt.Errorf("部署不存在或验签失败: %w", err)
	}
	deployedKey := connect.ApplicationKey(record.AppPublicKeyHex)
	if deployedKey == "" {
		return fmt.Errorf("部署公钥非法")
	}
	if expected := connect.ApplicationKey(expectedAppPublicKeyHex); expectedAppPublicKeyHex != "" {
		if expected == "" {
			return fmt.Errorf("目录启动 App 公钥非法")
		}
		if expected != deployedKey {
			return fmt.Errorf("部署公钥与目录目标不一致")
		}
	}
	if record.Indexseedhashhex != seed {
		return fmt.Errorf("部署 seed 与目录目标不一致")
	}
	if record.PublicationID == "" {
		return fmt.Errorf("部署证明摘要缺失")
	}
	expectedPublicationID, err := d.connectMgr.Directory().FilesPublicationID(deployedKey, seed)
	if err != nil {
		return err
	}
	if record.PublicationID != expectedPublicationID {
		return fmt.Errorf("部署证明与目录发布不一致")
	}
	return nil
}

func (d *Daemon) opConnectDeploymentInstall(ctx context.Context, p kmapi.ConnectDeploymentInstallRequest) (any, *kmapi.ErrorDetail) {
	if e := d.deploymentAccess(ctx); e != nil {
		return nil, e
	}
	r, err := d.deployments.Install(ctx, p.Indexseedhashhex)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	// 安装验证只产出部署登记，不写目录数据；目录条目由市场同步与手工
	// 来源提交（connect.market.*）负责。
	return struct {
		kmapi.ConnectDeploymentInstallResponse
		AppVersion string `json:"appVersion"`
	}{kmapi.ConnectDeploymentInstallResponse{DeploymentID: r.DeploymentID, AppPublicKeyHex: r.AppPublicKeyHex, Indexseedhashhex: r.Indexseedhashhex}, r.AppVersion}, nil
}
func (d *Daemon) opConnectDeploymentStart(ctx context.Context, p kmapi.ConnectDeploymentStartRequest) (any, *kmapi.ErrorDetail) {
	if e := d.deploymentAccess(ctx); e != nil {
		return nil, e
	}
	// 目录启动传 App 公钥和所选 seed，由 Go 在启动服务前核对当前目录与实际
	// 部署；独立启动省略该字段，沿用原有整包验证，不查目录。
	if expected := stringOrEmpty(p.ExpectedAppPublicKeyHex); expected != "" {
		if err := d.verifyDirectoryFilesDeployment(expected, p.DeploymentID); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	}
	r, err := d.deployments.Start(p.DeploymentID)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return struct {
		kmapi.ConnectDeploymentStartResponse
		AppVersion string `json:"appVersion"`
	}{kmapi.ConnectDeploymentStartResponse{DeploymentID: r.DeploymentID, Origin: r.Origin, URL: r.URL, AppPublicKeyHex: r.AppPublicKeyHex, Indexseedhashhex: r.Indexseedhashhex}, r.AppVersion}, nil
}
func (d *Daemon) opConnectDeploymentStop(ctx context.Context, p kmapi.ConnectDeploymentStopRequest) (any, *kmapi.ErrorDetail) {
	if e := d.deploymentAccess(ctx); e != nil {
		return nil, e
	}
	if err := d.deployments.Stop(p.DeploymentID); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return kmapi.Ok{Ok: true}, nil
}
func (d *Daemon) opConnectDeploymentList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.deploymentAccess(ctx); e != nil {
		return nil, e
	}
	records, err := d.deployments.List()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return map[string]any{"deployments": records}, nil
}

// verifiedConnectSource 只信任当前监听登记或有效 URL 发布记录，绝不采用页面自报 Files 身份。
// 同一处理阶段解码一次并复用结果：连接授权、展示资料与历史记录共用这份已验证身份，
// 不写目录数据。
func (d *Daemon) verifiedConnectSource(params map[string]any, origin string) (string, string, apppublication.AppDisplay, error) {
	if r, ok := d.deployments.Identity(origin); ok {
		return r.AppPublicKeyHex, r.DeploymentID, r.Display, nil
	}
	if d.deployments.WasDeploymentOrigin(origin) {
		return "", "", apppublication.AppDisplay{}, fmt.Errorf("Files deployment origin is no longer active")
	}
	p, present, err := connect.VerifiedPublication(params, origin)
	if err != nil {
		return "", "", apppublication.AppDisplay{}, err
	}
	if !present {
		return "", "", apppublication.AppDisplay{}, fmt.Errorf("verified App identity is required")
	}
	return p.AppPublicKeyHex, "", p.Display(), nil
}

func (d *Daemon) deploymentGeneration(origin string) string {
	if r, ok := d.deployments.Identity(origin); ok {
		return r.Generation
	}
	return ""
}
