package daemon

import (
	"context"
	"encoding/json"
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
func (d *Daemon) opConnectDeploymentInstall(ctx context.Context, p kmapi.ConnectDeploymentInstallRequest) (any, *kmapi.ErrorDetail) {
	if e := d.deploymentAccess(ctx); e != nil {
		return nil, e
	}
	r, err := d.deployments.Install(ctx, p.Indexseedhashhex)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	if err := d.connectMgr.Directory().ResolveSeed(r.Indexseedhashhex, r.AppPublicKeyHex, r.Display); err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return kmapi.ConnectDeploymentInstallResponse{DeploymentID: r.DeploymentID, AppPublicKeyHex: r.AppPublicKeyHex, Indexseedhashhex: r.Indexseedhashhex}, nil
}
func (d *Daemon) opConnectDeploymentStart(ctx context.Context, p kmapi.ConnectDeploymentStartRequest) (any, *kmapi.ErrorDetail) {
	if e := d.deploymentAccess(ctx); e != nil {
		return nil, e
	}
	r, err := d.deployments.Start(p.DeploymentID)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return kmapi.ConnectDeploymentStartResponse{DeploymentID: r.DeploymentID, Origin: r.Origin, URL: r.URL, AppPublicKeyHex: r.AppPublicKeyHex, Indexseedhashhex: r.Indexseedhashhex}, nil
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

// verifiedConnectIdentity 只信任当前监听登记或有效 URL proof，绝不采用页面自报 Files 身份。
func (d *Daemon) verifiedConnectIdentity(params map[string]any, origin string) (string, string, error) {
	if r, ok := d.deployments.Identity(origin); ok {
		return r.AppPublicKeyHex, r.DeploymentID, nil
	}
	if d.deployments.WasDeploymentOrigin(origin) {
		return "", "", fmt.Errorf("Files deployment origin is no longer active")
	}
	key, present, err := connect.VerifyLoginPublicationProof(params, origin)
	if err != nil {
		return "", "", err
	}
	if !present {
		return "", "", fmt.Errorf("verified App identity is required")
	}
	return key, "", nil
}

func (d *Daemon) deploymentGeneration(origin string) string {
	if r, ok := d.deployments.Identity(origin); ok {
		return r.Generation
	}
	return ""
}

func (d *Daemon) verifiedConnectDisplay(params map[string]any, origin string) (apppublication.AppDisplay, error) {
	if r, ok := d.deployments.Identity(origin); ok {
		return r.Display, nil
	}
	raw, err := json.Marshal(params["publicationProof"])
	if err != nil {
		return apppublication.AppDisplay{}, err
	}
	proof, err := apppublication.DecodeProofJSON(raw)
	if err != nil {
		return apppublication.AppDisplay{}, err
	}
	p, err := apppublication.VerifyPublication(proof, origin)
	if err != nil {
		return apppublication.AppDisplay{}, err
	}
	return p.Display(), nil
}
