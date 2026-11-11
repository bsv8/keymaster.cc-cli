package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// paymentRetryInterval 为待重试付款写入的重试间隔。只在队列非空时有定时器，
// 队列清空即停，不常驻 goroutine。
const paymentRetryInterval = 5 * time.Second

// paymentRetryOp 为一次待重试的付款记录写入：put 按快照条件写 next，
// delete 按快照条件删除占位。重试只写记录，绝不重新选币/签名/广播。
type paymentRetryOp struct {
	op   string // "put" | "delete"
	base connect.PaymentRecord
	next connect.PaymentRecord // op=="put" 时的目标记录
}

// enqueuePaymentWrite 将落盘失败的付款写入排队后台重试，并确保重试定时器已 armed。
// 调用方已因存储失败返回内部错误；App 重试同 ID 会读到旧记录，重试循环负责收敛。
// 已关闭（永久停止标记）则丢弃，不再接受新的重试，避免关闭后旧实例继续写盘。
func (d *Daemon) enqueuePaymentWrite(key string, op paymentRetryOp) {
	d.paymentRetryMu.Lock()
	defer d.paymentRetryMu.Unlock()
	if d.paymentRetryStopped {
		return
	}
	if d.paymentRetry == nil {
		d.paymentRetry = map[string]paymentRetryOp{}
	}
	d.paymentRetry[key] = op
	if d.paymentRetryTimer == nil {
		d.paymentRetryTimer = time.AfterFunc(paymentRetryInterval, func() { d.retryPaymentWrites() })
	}
}

// retryPaymentWrites 重试所有待写付款记录。成功或状态已推进（条件不匹配）则丢弃；
// 仍落盘失败则保留并重 armed 定时器。绝不重新广播。
// 已关闭时直接丢弃残留，不再写盘（未落盘的收尾由下次启动收敛接管）。
func (d *Daemon) retryPaymentWrites() {
	d.paymentRetryMu.Lock()
	if d.paymentRetryStopped {
		d.paymentRetry = map[string]paymentRetryOp{}
		d.paymentRetryTimer = nil
		d.paymentRetryMu.Unlock()
		return
	}
	if len(d.paymentRetry) == 0 {
		d.paymentRetryTimer = nil
		d.paymentRetryMu.Unlock()
		return
	}
	ops := d.paymentRetry
	d.paymentRetry = map[string]paymentRetryOp{}
	d.paymentRetryTimer = nil
	d.paymentRetryWg.Add(1)
	d.paymentRetryMu.Unlock()
	defer d.paymentRetryWg.Done()
	for key, op := range ops {
		var ok bool
		var err error
		if op.op == "delete" {
			ok, err = d.connectMgr.Payments().DeleteIfUnchanged(key, op.base)
		} else {
			ok, _, err = d.connectMgr.Payments().PutIfUnchanged(key, op.base, op.next)
		}
		if err != nil {
			// 存储仍不可用：保留重试，不覆盖、不广播。
			// 关闭流程会等待本轮结束；重 armed 由末尾统一按停止标记决定。
			d.paymentRetryMu.Lock()
			if d.paymentRetry == nil {
				d.paymentRetry = map[string]paymentRetryOp{}
			}
			d.paymentRetry[key] = op
			d.paymentRetryMu.Unlock()
			continue
		}
		if !ok {
			// 状态已被推进（终态/收敛/新占位）：丢弃本次重试意图，不覆盖。
			continue
		}
	}
	d.paymentRetryMu.Lock()
	if len(d.paymentRetry) > 0 && d.paymentRetryTimer == nil && !d.paymentRetryStopped {
		d.paymentRetryTimer = time.AfterFunc(paymentRetryInterval, func() { d.retryPaymentWrites() })
	}
	d.paymentRetryMu.Unlock()
}

// stopPaymentRetry 停止付款重试定时器并等待正在执行的重试结束，保证返回后
// 旧实例不再写付款文件；待重试项由下次启动收敛接管，不在关闭时补写。
func (d *Daemon) stopPaymentRetry() {
	d.paymentRetryMu.Lock()
	d.paymentRetryStopped = true
	if d.paymentRetryTimer != nil {
		d.paymentRetryTimer.Stop()
		d.paymentRetryTimer = nil
	}
	d.paymentRetryMu.Unlock()
	d.paymentRetryWg.Wait()
}

// heldPayment 为冻结的付款计划：批准前选币并签名保存，批准后原样广播，不重新选币。
type heldPayment struct {
	spend   *wallet.SpendResult
	wallet  *wallet.Wallet
	release func()
}

func (d *Daemon) heldPayments() map[string]*heldPayment {
	d.paymentsMu.Lock()
	defer d.paymentsMu.Unlock()
	if d.paymentHeld == nil {
		d.paymentHeld = map[string]*heldPayment{}
	}
	return d.paymentHeld
}

func (d *Daemon) getHeld(key string) (*heldPayment, bool) {
	d.paymentsMu.Lock()
	defer d.paymentsMu.Unlock()
	h, ok := d.paymentHeld[key]
	return h, ok
}

func (d *Daemon) putHeld(key string, h *heldPayment) {
	d.paymentsMu.Lock()
	defer d.paymentsMu.Unlock()
	if d.paymentHeld == nil {
		d.paymentHeld = map[string]*heldPayment{}
	}
	d.paymentHeld[key] = h
}

func (d *Daemon) deleteHeld(key string) (*heldPayment, bool) {
	d.paymentsMu.Lock()
	defer d.paymentsMu.Unlock()
	h, ok := d.paymentHeld[key]
	if ok {
		delete(d.paymentHeld, key)
	}
	return h, ok
}

// parsePaymentPayParams 只做必要结构检查：outputs 非空、金额非负安全整数、script 偶数长度 hex。
// 允许零金额与无法识别的脚本；不做白名单、dust 或脚本执行验收。空脚本允许（零长度锁定脚本）。
func parsePaymentPayParams(params map[string]any) (string, []connect.PaymentOutput, error) {
	rawID, ok := params["clientRequestId"]
	if !ok {
		return "", nil, fmt.Errorf("clientRequestId 必填")
	}
	clientRequestID, ok := rawID.(string)
	if !ok || strings.TrimSpace(clientRequestID) == "" {
		return "", nil, fmt.Errorf("clientRequestId 非法")
	}
	if len(clientRequestID) > 128 {
		return "", nil, fmt.Errorf("clientRequestId 非法")
	}
	for _, c := range clientRequestID {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return "", nil, fmt.Errorf("clientRequestId 非法")
		}
	}
	rawOutputs, ok := params["outputs"]
	if !ok {
		return "", nil, fmt.Errorf("outputs 必填")
	}
	arr, ok := rawOutputs.([]any)
	if !ok || len(arr) == 0 {
		return "", nil, fmt.Errorf("outputs 必须是非空数组")
	}
	outputs := make([]connect.PaymentOutput, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("outputs[%d] 非法", i)
		}
		if len(m) != 2 {
			// 只允许 amount/script；多余字段拒绝。
			if _, hasAmount := m["amount"]; !hasAmount {
				return "", nil, fmt.Errorf("outputs[%d].amount 必填", i)
			}
			if _, hasScript := m["script"]; !hasScript {
				return "", nil, fmt.Errorf("outputs[%d].script 必填", i)
			}
			for k := range m {
				if k != "amount" && k != "script" {
					return "", nil, fmt.Errorf("outputs[%d] 含不支持字段", i)
				}
			}
		}
		amountRaw, ok := m["amount"]
		if !ok {
			return "", nil, fmt.Errorf("outputs[%d].amount 必填", i)
		}
		var amount uint64
		switch v := amountRaw.(type) {
		case float64:
			if v != float64(int64(v)) || v < 0 || v > 9007199254740991 {
				return "", nil, fmt.Errorf("outputs[%d].amount 必须是非负安全整数", i)
			}
			amount = uint64(v)
		case int:
			if v < 0 {
				return "", nil, fmt.Errorf("outputs[%d].amount 必须是非负安全整数", i)
			}
			amount = uint64(v)
		case int64:
			if v < 0 || v > 9007199254740991 {
				return "", nil, fmt.Errorf("outputs[%d].amount 必须是非负安全整数", i)
			}
			amount = uint64(v)
		default:
			return "", nil, fmt.Errorf("outputs[%d].amount 必须是非负安全整数", i)
		}
		scriptRaw, ok := m["script"]
		if !ok {
			return "", nil, fmt.Errorf("outputs[%d].script 必填", i)
		}
		scriptHex, ok := scriptRaw.(string)
		if !ok {
			return "", nil, fmt.Errorf("outputs[%d].script 必须是 hex", i)
		}
		if len(scriptHex)%2 != 0 {
			return "", nil, fmt.Errorf("outputs[%d].script 必须是偶数长度 hex", i)
		}
		if len(scriptHex) > 0 {
			for _, c := range scriptHex {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
					return "", nil, fmt.Errorf("outputs[%d].script 必须是 hex", i)
				}
			}
		}
		lowered := strings.ToLower(scriptHex)
		outputs = append(outputs, connect.PaymentOutput{Amount: amount, Script: lowered})
	}
	return clientRequestID, outputs, nil
}

func paymentAssetID(network string) string {
	if network == "main" {
		return "bsv-mainnet"
	}
	return "bsv-testnet"
}

func paymentResultFromRecord(r connect.PaymentRecord) map[string]any {
	out := map[string]any{
		"paymentId":       r.PaymentID,
		"clientRequestId": r.ClientRequestID,
		"status":          string(r.Status),
		"assetId":         paymentAssetID(r.Network),
	}
	if r.Txid != "" && (r.Status == connect.PaymentAccepted || r.Status == connect.PaymentAlreadyKnown) {
		out["txid"] = r.Txid
	}
	if r.Status != connect.PaymentPending {
		// fee/change 仅在冻结后才有；pending 时省略。
		out["feeSatoshis"] = r.FeeSats
		out["changeSatoshis"] = r.ChangeSats
	}
	if r.Reason != "" {
		// 对外只保留稳定原因；cancelled/input-conflict 透出，其他统一为 payment-unresolved。
		if r.Reason == "cancelled" || r.Reason == "input-conflict" {
			out["reason"] = r.Reason
		} else if r.Status == connect.PaymentFailed || r.Status == connect.PaymentUnknown || r.Status == connect.PaymentNotSigned {
			out["reason"] = "payment-unresolved"
		} else {
			out["reason"] = r.Reason
		}
	}
	return out
}

// opConnectPaymentPay 实现 payment.pay：幂等落库 → 冻结输入和保存预览 → 审批 → 提交。
// 首次调用冻结并创建审批返回 approvalId；批准后经 approvalId 认领执行原计划，不重新选币。
func (d *Daemon) opConnectPaymentPay(ctx context.Context, auth connect.Auth, params map[string]any, origin string) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if d.store == nil {
		return nil, nil, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	if d.isStopping() {
		return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
	}
	d.mu.Lock()
	locked := d.state != StateUnlocked
	d.mu.Unlock()
	if locked {
		return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	clientRequestID, outputs, err := parsePaymentPayParams(params)
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	info, settings, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	network := info.BsvNetwork
	// 占位前复制解锁私钥：为空则直接拒绝，不创建占位，避免留下无冻结的游离 pending。
	d.mu.Lock()
	priv := append([]byte(nil), d.privateKey...)
	d.mu.Unlock()
	if len(priv) == 0 {
		return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	defer func() {
		for i := range priv {
			priv[i] = 0
		}
	}()
	identityKey := connect.PaymentIdentityKey(network, origin, auth.WalletPublicKey, auth.AppID, auth.AppPublicKey)
	key := connect.PaymentRequestKey(identityKey, clientRequestID)
	assetID := paymentAssetID(network)
	digest := connect.PaymentRequestDigest(identityKey, clientRequestID, outputs, assetID)
	// 原子占住请求 ID：并发相同 ID 只有一者能进入选币/冻结，后到者直接返回已有记录，
	// 不重复创建计划，不覆盖先占者的付款计划。
	claimedPlaceholder := connect.PaymentRecord{
		Key: key, PaymentID: newPaymentID(), ClientRequestID: clientRequestID,
		Origin: origin, OwnerPublicKeyHex: auth.WalletPublicKey, Network: network,
		AppID: auth.AppID, AppPublicKey: auth.AppPublicKey,
		AuthorizationID: auth.AuthorizationID, IdentityDigest: auth.IdentityDigest,
		Requirements: append([]string(nil), auth.Requirements...),
		Generation: d.connectMgr.Generation(),
		RequestDigest: digest, Outputs: outputs,
		Status: connect.PaymentPending,
	}
	created, existing, claimErr := d.connectMgr.Payments().Claim(claimedPlaceholder, 64)
	if claimErr != nil {
		if claimErr.Error() == "payment-pending-limit" {
			return nil, nil, errDetail(kmapi.ErrStateConflict, "待处理付款过多")
		}
		return nil, nil, errDetail(kmapi.ErrInternal, claimErr.Error())
	}
	if !created {
		// 已存在：同 ID 换内容报冲突，否则返回原记录（不重复冻结，不新建审批）。
		if existing.RequestDigest != digest {
			return nil, nil, errDetail(kmapi.ErrStateConflict, "clientRequestId 已使用且内容不同")
		}
		return paymentResultFromRecord(existing), nil, nil
	}
	// 本次占位成功，后续冻结/审批/提交都绑定同一 Key 与 RequestDigest。
	paymentID := claimedPlaceholder.PaymentID
	// 冻结：使用钱包选币能力构造完整 outputs + 找零，费用按全部脚本实际长度计算，余额需覆盖总额+矿工费。
	// priv 已在占位前复制并校验非空，此处直接使用占位时快照的授权与网络，不重新读取可变状态。
	wocClient := woc.New(wocBaseURL(network), settings.WOCAPIKey)
	w, releaseWallet, err := wallet.Shared(wocClient, wallet.Options{
		Network: network, PrivateKey: priv,
		FeeRateSatoshisPerKb: settings.FeeRateSatoshisPerKb,
	})
	if err != nil {
		// 共享钱包打开失败：冻结从未开始，条件删除占位以便重试，不留下游离 pending；
		// 删除落盘失败则排队后台重试，调用方仍按内部错误处理。
		if ok, derr := d.connectMgr.Payments().DeleteIfUnchanged(key, claimedPlaceholder); derr != nil {
			d.enqueuePaymentWrite(key, paymentRetryOp{op: "delete", base: claimedPlaceholder})
		} else if !ok {
			// 占位已被推进（理论上仅重启收敛可能）：返回当前记录，不覆盖。
			if curRec, found := d.connectMgr.Payments().Get(key); found {
				return paymentResultFromRecord(curRec), nil, nil
			}
		}
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	// 构造业务输出：顺序即链上顺序，找零由钱包生成。
	// parse 已校验偶数 hex，此处解码理论上不失败；若失败则更新占位为明确未提交，不留游离 pending。
	// 存储失败时返回内部错误，不按成功处理。
	walletOutputs := make([]wallet.Output, 0, len(outputs))
	for _, o := range outputs {
		var scriptBytes []byte
		if o.Script != "" {
			b, herr := hex.DecodeString(o.Script)
			if herr != nil {
				releaseWallet()
				if res, perr := d.failClaimedNotSigned(key, claimedPlaceholder, "script 非法"); perr != nil {
					return nil, nil, perr
				} else {
					return res, nil, nil
				}
			}
			scriptBytes = b
		}
		// 空脚本编码为零长度锁定脚本，不因无法解释而拒绝。
		locking := script.NewFromBytes(scriptBytes)
		if locking == nil {
			releaseWallet()
			if res, perr := d.failClaimedNotSigned(key, claimedPlaceholder, "script 非法"); perr != nil {
				return nil, nil, perr
			} else {
				return res, nil, nil
			}
		}
		walletOutputs = append(walletOutputs, wallet.Output{Satoshis: o.Amount, LockingScript: locking})
	}
	feeRate := settings.FeeRateSatoshisPerKb
	if feeRate <= 0 {
		feeRate = 1000
	}
	spend, err := w.BuildWithFeeRate(ctx, walletOutputs, feeRate)
	if err != nil {
		releaseWallet()
		msg := err.Error()
		reason := msg
		if strings.Contains(strings.ToLower(msg), "insufficient") || strings.Contains(strings.ToLower(msg), "no-utxos") || strings.Contains(strings.ToLower(msg), "no utxo") {
			reason = "insufficient-balance"
		}
		// 余额不足、构造失败：明确未提交。复用占位记录的同一 PaymentID 与授权快照，不新建记录。
		// 条件更新：占位期间被改写则返回当前记录，不覆盖；存储失败排队后台重试并返回内部错误。
		failRec := claimedPlaceholder
		failRec.Status = connect.PaymentNotSigned
		failRec.Reason = reason
		if ok, _, uerr := d.connectMgr.Payments().PutIfUnchanged(key, claimedPlaceholder, failRec); uerr != nil {
			d.enqueuePaymentWrite(key, paymentRetryOp{op: "put", base: claimedPlaceholder, next: failRec})
			return nil, nil, errDetail(kmapi.ErrInternal, uerr.Error())
		} else if !ok {
			if curRec, found := d.connectMgr.Payments().Get(key); found {
				out := paymentResultFromRecord(curRec)
				if curRec.Reason != "cancelled" && curRec.Reason != "input-conflict" {
					out["reason"] = "payment-unresolved"
				}
				return out, nil, nil
			}
			return nil, nil, errDetail(kmapi.ErrInternal, "付款状态更新失败")
		}
		out := paymentResultFromRecord(failRec)
		// 对外原因收敛为稳定码，本地保留诊断。
		if reason != "cancelled" && reason != "input-conflict" {
			out["reason"] = "payment-unresolved"
		}
		return out, nil, nil
	}
	// 冻结成功：保存预览（原计划）。复用占位记录的同一 PaymentID，只补充冻结值，
	// 不替换 Key/Digest/授权快照；条件更新防止旧快照覆盖后续状态。
	frozenRec := claimedPlaceholder
	frozenRec.Status = connect.PaymentPending
	frozenRec.Txid = spend.TxID
	frozenRec.FeeSats = spend.Fee
	frozenRec.ChangeSats = spend.Change
	frozenRec.RawHex = spend.RawHex
	if ok, cur, err := d.connectMgr.Payments().PutIfUnchanged(key, claimedPlaceholder, frozenRec); err != nil {
		_ = w.Release(spend)
		releaseWallet()
		// 冻结更新落盘失败：条件删除占位以便重试（防误删后续状态），删不动则排队后台重试。
		if ok, derr := d.connectMgr.Payments().DeleteIfUnchanged(key, claimedPlaceholder); derr != nil {
			d.enqueuePaymentWrite(key, paymentRetryOp{op: "delete", base: claimedPlaceholder})
		} else if !ok {
			if curRec, found := d.connectMgr.Payments().Get(key); found {
				return paymentResultFromRecord(curRec), nil, nil
			}
		}
		_ = cur
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	} else if !ok {
		_ = w.Release(spend)
		releaseWallet()
		// 占位期间记录被改写（终态或收敛）：返回当前记录，不覆盖。
		return paymentResultFromRecord(cur), nil, nil
	}
	d.putHeld(key, &heldPayment{spend: spend, wallet: w, release: releaseWallet})
	// 先创建暂存审批（不可见、不可决定），持久化绑定成功后再发布可见。
	// 审批 params 绑定 paymentKey、requestDigest 与冻结值，执行时复核一致，
	// 计划被替换则拒绝，要求重新确认。
	staged := d.connectMgr.Queue().CreateStaged(
		auth.AppPublicKey, auth.AppID, auth.AppName,
		"payment.pay",
		map[string]any{"clientRequestId": clientRequestID, "paymentKey": key, "requestDigest": digest, "outputs": outputsToMaps(outputs), "feeSatoshis": spend.Fee, "changeSatoshis": spend.Change, "txid": spend.TxID},
		origin, auth.Requirements, auth.IdentityDigest,
	)
	// 回写 ApprovalID，使重启收敛可区分“审批从未创建”与“审批可能已执行”。
	// 条件更新：期间记录被改写则放弃本次审批，禁止旧快照覆盖后续状态。
	// 存储失败则排队删除重试（条件删除，防误删后续状态），调用方返回内部错误。
	approvedRec := frozenRec
	approvedRec.ApprovalID = staged.ID
	if ok, cur, err := d.connectMgr.Payments().PutIfUnchanged(key, frozenRec, approvedRec); err != nil {
		d.connectMgr.Queue().Discard(staged.ID)
		_ = w.Release(spend)
		if held, has := d.deleteHeld(key); has && held != nil {
			held.release()
		}
		// 绑定回写落盘失败：条件删除占位以便重试（防误删后续状态），
		// 删不动则排队后台重试；调用方返回内部错误，绝不在绑定未落盘时发布审批。
		if ok, derr := d.connectMgr.Payments().DeleteIfUnchanged(key, frozenRec); derr != nil {
			d.enqueuePaymentWrite(key, paymentRetryOp{op: "delete", base: frozenRec})
		} else if !ok {
			_ = cur
		}
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	} else if !ok {
		d.connectMgr.Queue().Discard(staged.ID)
		_ = w.Release(spend)
		if held, has := d.deleteHeld(key); has && held != nil {
			held.release()
		}
		// 记录已被并发推进：返回当前记录，不覆盖。
		_ = cur
		if curRec, found := d.connectMgr.Payments().Get(key); found {
			return paymentResultFromRecord(curRec), nil, nil
		}
		return nil, nil, errDetail(kmapi.ErrInternal, "付款计划已变化，需重新确认")
	}
	// 绑定已落盘，再让审批可见。Publish 仅内存操作，失败属于逻辑错误，按内部错误处理；
	// 记录按条件删除（防误删后续状态），删不动则排队后台重试。
	if _, published := d.connectMgr.Queue().Publish(staged.ID); !published {
		d.connectMgr.Queue().Discard(staged.ID)
		_ = w.Release(spend)
		if held, has := d.deleteHeld(key); has && held != nil {
			held.release()
		}
		if ok, derr := d.connectMgr.Payments().DeleteIfUnchanged(key, approvedRec); derr != nil {
			d.enqueuePaymentWrite(key, paymentRetryOp{op: "delete", base: approvedRec})
		} else if !ok {
			if curRec, found := d.connectMgr.Payments().Get(key); found {
				return paymentResultFromRecord(curRec), nil, nil
			}
		}
		return nil, nil, errDetail(kmapi.ErrInternal, "审批发布失败")
	}
	return map[string]any{"approvalId": staged.ID, "status": "pending", "paymentId": paymentID}, nil, nil
}

func outputsToMaps(outputs []connect.PaymentOutput) []any {
	out := make([]any, 0, len(outputs))
	for _, o := range outputs {
		out = append(out, map[string]any{"amount": o.Amount, "script": o.Script})
	}
	return out
}

// failClaimedNotSigned 将占位记录条件收敛为未提交（复用同一 PaymentID 与授权快照），
// 并返回对外展示结果。占位期间被改写则返回当前记录，不覆盖。
// 存储失败时排队后台重试并返回内部错误，调用方不得按成功处理。
func (d *Daemon) failClaimedNotSigned(key string, base connect.PaymentRecord, reason string) (map[string]any, *kmapi.ErrorDetail) {
	failRec := base
	failRec.Status = connect.PaymentNotSigned
	failRec.Reason = reason
	if ok, _, err := d.connectMgr.Payments().PutIfUnchanged(key, base, failRec); err != nil {
		d.enqueuePaymentWrite(key, paymentRetryOp{op: "put", base: base, next: failRec})
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	} else if !ok {
		if curRec, found := d.connectMgr.Payments().Get(key); found {
			out := paymentResultFromRecord(curRec)
			if curRec.Reason != "cancelled" && curRec.Reason != "input-conflict" {
				out["reason"] = "payment-unresolved"
			}
			return out, nil
		}
		return nil, errDetail(kmapi.ErrInternal, "付款状态更新失败")
	}
	out := paymentResultFromRecord(failRec)
	out["reason"] = "payment-unresolved"
	return out, nil
}

func newPaymentID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "pay_" + hex.EncodeToString(b)
}

// execPaymentApproved 执行已批准的付款：复核原授权及运行代际后，对原计划广播，不重新选币。
// 审批绑定不可替换的付款记录与计划摘要：核对 paymentKey、requestDigest 与冻结值，
// 计划被替换则拒绝，要求重新确认。
func (d *Daemon) execPaymentApproved(ctx context.Context, approval connect.Approval) (any, map[string][]byte, *kmapi.ErrorDetail) {
	params := approval.Params
	keyRaw, _ := params["paymentKey"].(string)
	if keyRaw == "" {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "审批缺少付款引用")
	}
	rec, ok := d.connectMgr.Payments().Get(keyRaw)
	if !ok {
		return nil, nil, errDetail(kmapi.ErrNotFound, "付款不存在")
	}
	// 复核原授权与运行代际，并核对审批与付款记录的绑定。不借新授权继续旧申请。
	// 广播前确定无法继续（授权失效、绑定破裂）时，持久收敛为未提交并释放冻结，
	// 再返回错误；未知结果的责任继续保留。
	auth, perr := d.validatePaymentAuth(rec, approval)
	if perr != nil {
		d.convergePaymentNotSigned(rec, "auth-invalid")
		return nil, nil, perr
	}
	_ = auth
	// 运行代际已在 validatePaymentAuth 中校验。
	if rec.Status == connect.PaymentAccepted || rec.Status == connect.PaymentAlreadyKnown || rec.Status == connect.PaymentFailed || rec.Status == connect.PaymentNotSigned {
		return paymentResultFromRecord(rec), nil, nil
	}
	// 审批必须绑定创建时的冻结计划；缺绑定字段的旧审批直接拒绝，需重新确认。
	// 绑定破裂属于 plan 侧确定失败，同样收敛为未提交并释放冻结（见上）。
	if !checkApprovalPlanBinding(approval, rec) {
		d.convergePaymentNotSigned(rec, "plan-changed")
		return nil, nil, errDetail(kmapi.ErrStateConflict, "付款计划已变化，需重新确认")
	}
	// 持久记录必须已载有 ApprovalID 才允许广播（创建时先落盘绑定再发布审批）。
	// 缺失说明绑定回写失败：禁止广播，收敛为未提交并释放冻结，不留“可能提交”悬置。
	if rec.ApprovalID == "" {
		d.convergePaymentNotSigned(rec, "binding-missing")
		return nil, nil, errDetail(kmapi.ErrInternal, "付款审批绑定缺失，需重新确认")
	}
	if rec.ApprovalID != approval.ID {
		return nil, nil, errDetail(kmapi.ErrStateConflict, "付款计划已变化，需重新确认")
	}
	held, hasHeld := d.getHeld(keyRaw)
	var rawHex string
	var txid string
	if hasHeld && held != nil && held.spend != nil {
		// held 计划必须与持久冻结一致，否则拒绝执行。
		if held.spend.RawHex != rec.RawHex || held.spend.TxID != rec.Txid {
			return nil, nil, errDetail(kmapi.ErrStateConflict, "付款计划已变化，需重新确认")
		}
		rawHex = held.spend.RawHex
		txid = held.spend.TxID
	} else {
		rawHex = rec.RawHex
		txid = rec.Txid
	}
	if rawHex == "" {
		return nil, nil, errDetail(kmapi.ErrInternal, "冻结计划缺失")
	}
	// 广播前复核已完成；此处不再重新选币。
	var broadcastErr error
	var broadcastTxid string
	if hasHeld && held != nil && held.spend != nil {
		broadcastTxid, broadcastErr = held.wallet.BroadcastPrepared(ctx, "connect-payment", held.spend)
		// BroadcastPrepared 成功后释放 Shared 引用（分配保持锁定表示已花费）；失败时其内部已释放分配。
		held.release()
		d.deleteHeld(keyRaw)
	} else {
		// held 丢失（如进程内清理）：用持久 rawHex 直接广播，不重新选币。
		// 重启后的恢复不走此分支——重启收敛已将遗留 pending 转为终态，此处仅覆盖同进程丢失场景。
		info, settings, serr := d.store.Snapshot()
		if serr != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, serr.Error())
		}
		wocClient := woc.New(wocBaseURL(info.BsvNetwork), settings.WOCAPIKey)
		broadcastTxid, broadcastErr = wocClient.Broadcast(ctx, rawHex)
		_ = txid
	}
	if broadcastErr != nil {
		status, reason := classifyBroadcastErr(broadcastErr)
		updated := rec
		updated.Status = status
		updated.Reason = reason
		// 条件更新：期间被并发推进则返回当前记录，不覆盖；
		// 存储失败则排队后台重试，并返回已取得的广播结果（不得把旧 pending 当作执行结果缓存）。
		if ok, _, err := d.connectMgr.Payments().PutIfUnchanged(keyRaw, rec, updated); err != nil {
			d.enqueuePaymentWrite(keyRaw, paymentRetryOp{op: "put", base: rec, next: updated})
			return paymentResultFromRecord(updated), nil, nil
		} else if !ok {
			if curRec, found := d.connectMgr.Payments().Get(keyRaw); found {
				return paymentResultFromRecord(curRec), nil, nil
			}
			return nil, nil, errDetail(kmapi.ErrInternal, "付款状态更新失败")
		}
		rec = updated
		return paymentResultFromRecord(rec), nil, nil
	}
	// 广播接受或同笔交易已知：成功，返回 txid。
	_ = broadcastTxid
	updated := rec
	updated.Status = connect.PaymentAccepted
	if broadcastTxid != "" {
		updated.Txid = normalizeTxid(broadcastTxid)
	} else if updated.Txid == "" {
		updated.Txid = txid
	}
	updated.Reason = ""
	if ok, _, err := d.connectMgr.Payments().PutIfUnchanged(keyRaw, rec, updated); err != nil {
		// 落盘失败：排队后台重试，返回已取得的广播结果（不得把旧 pending 当作执行结果缓存）。
		d.enqueuePaymentWrite(keyRaw, paymentRetryOp{op: "put", base: rec, next: updated})
		return paymentResultFromRecord(updated), nil, nil
	} else if !ok {
		if curRec, found := d.connectMgr.Payments().Get(keyRaw); found {
			return paymentResultFromRecord(curRec), nil, nil
		}
		return nil, nil, errDetail(kmapi.ErrInternal, "付款状态更新失败")
	}
	rec = updated
	return paymentResultFromRecord(rec), nil, nil
}

// convergePaymentNotSigned 将广播前确定无法继续的申请持久收敛为未提交并释放
// 原冻结资源（held 输入与 Shared 引用），再由调用方返回对应错误。
// 仅用于广播尚未发生的确定性失败；广播结果未知时不得调用（责任继续保留）。
// 条件更新：记录已被并发推进则不覆盖，直接返回。仅 pending 可收敛，
// 已终态（accepted/failed/not_signed/unknown）绝不回退。
// 存储失败时记录保持原状（已回滚），但冻结必须释放，否则资金在当前进程
// 持续冻结；调用方仍返回原错误，不静默跳过。
func (d *Daemon) convergePaymentNotSigned(rec connect.PaymentRecord, reason string) {
	if rec.Status != connect.PaymentPending {
		return
	}
	updated := rec
	updated.Status = connect.PaymentNotSigned
	updated.Reason = reason
	ok, _, err := d.connectMgr.Payments().PutIfUnchanged(rec.Key, rec, updated)
	if err != nil {
		// 落盘失败：记录未变（已回滚），排队后台重试收敛；但冻结必须立即释放，
		// 否则资金在当前进程持续冻结。调用方仍返回原错误，不静默跳过。
		d.enqueuePaymentWrite(rec.Key, paymentRetryOp{op: "put", base: rec, next: updated})
		if held, has := d.deleteHeld(rec.Key); has && held != nil {
			_ = held.wallet.Release(held.spend)
			held.release()
		}
		return
	}
	if !ok {
		return
	}
	if held, has := d.deleteHeld(rec.Key); has && held != nil {
		_ = held.wallet.Release(held.spend)
		held.release()
	}
}

// validatePaymentAuth 复核原授权及运行代际，不借新授权继续旧申请。
// 必须精确绑定创建时的 authorizationId，并核对 AppID、身份摘要、requirements、
// 来源、网络、钱包与代际；任一变化都拒绝，原申请责任仍保留，需重新登录后用新 ID 申请。
func (d *Daemon) validatePaymentAuth(rec connect.PaymentRecord, approval connect.Approval) (connect.Auth, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return connect.Auth{}, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if rec.AuthorizationID == "" {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	a, ok := d.connectMgr.AuthStore().FindByID(rec.AuthorizationID)
	if !ok {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	// 审批必须属于同一授权与来源。
	if approval.AppPublicKey != a.AppPublicKey || approval.Origin != a.Origin || approval.AppID != a.AppID {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	if approval.IdentityDigest != "" && approval.IdentityDigest != a.IdentityDigest {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	// 付款记录必须属于原授权身份。
	if rec.Origin != a.Origin || !strings.EqualFold(rec.AppPublicKey, a.AppPublicKey) || rec.AppID != a.AppID {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	if rec.IdentityDigest != "" && rec.IdentityDigest != a.IdentityDigest {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	// requirements：付款需要 private-key，原授权必须仍具备。
	hasPrivateKey := false
	for _, r := range a.Requirements {
		if r == "private-key" {
			hasPrivateKey = true
			break
		}
	}
	if !hasPrivateKey {
		return connect.Auth{}, errDetail(kmapi.ErrForbidden, "无 private-key 操作许可")
	}
	if a.Generation == "" || a.Generation != d.connectMgr.Generation() {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已过期，请重新登录")
	}
	if a.Generation != rec.Generation {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已过期，请重新登录")
	}
	if d.store == nil {
		return connect.Auth{}, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return connect.Auth{}, errDetail(kmapi.ErrInternal, err.Error())
	}
	d.mu.Lock()
	walletPub := d.publicKey
	d.mu.Unlock()
	if a.Network != info.BsvNetwork || !strings.EqualFold(a.WalletPublicKey, walletPub) {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	if a.Network != rec.Network || !strings.EqualFold(a.WalletPublicKey, rec.OwnerPublicKeyHex) {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	return a, nil
}

// checkApprovalPlanBinding 核对审批与付款记录的计划绑定：同一 Key、同一请求摘要、
// 同一冻结值（txid/费用/找零）。缺字段的旧审批或任何不一致都拒绝，要求重新确认，
// 不悄悄按新计划执行。
func checkApprovalPlanBinding(approval connect.Approval, rec connect.PaymentRecord) bool {
	params := approval.Params
	if params == nil {
		return false
	}
	if keyRaw, _ := params["paymentKey"].(string); keyRaw == "" || keyRaw != rec.Key {
		return false
	}
	digestRaw, _ := params["requestDigest"].(string)
	if digestRaw == "" || digestRaw != rec.RequestDigest {
		return false
	}
	txidRaw, _ := params["txid"].(string)
	if txidRaw == "" || !strings.EqualFold(txidRaw, rec.Txid) {
		return false
	}
	feeRaw, ok := params["feeSatoshis"]
	if !ok || !uint64Equal(feeRaw, rec.FeeSats) {
		return false
	}
	changeRaw, ok := params["changeSatoshis"]
	if !ok || !uint64Equal(changeRaw, rec.ChangeSats) {
		return false
	}
	return true
}

func uint64Equal(v any, want uint64) bool {
	switch n := v.(type) {
	case uint64:
		return n == want
	case uint32:
		return uint64(n) == want
	case int:
		return n >= 0 && uint64(n) == want
	case int64:
		return n >= 0 && uint64(n) == want
	case float64:
		return n == float64(int64(n)) && n >= 0 && uint64(n) == want
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i >= 0 && uint64(i) == want
		}
		return false
	default:
		return false
	}
}

// classifyBroadcastErr 统一映射广播结果，优先读取结构化回执，兼容 errors.Join 包装：
//   - Receipt.Status == "failed"：明确拒绝 → failed；
//   - 无错误（accepted/already-known）：调用方已直接成功，不进入此函数；
//   - 其他（含 unknown 回执、超时/断连、钱包内部错误）：未知，不自动重付。
func classifyBroadcastErr(err error) (connect.PaymentStatus, string) {
	var be *woc.BroadcastError
	if errors.As(err, &be) {
		switch be.Receipt.Status {
		case "failed":
			return connect.PaymentFailed, truncateReason(err.Error())
		case "already-known":
			return connect.PaymentAccepted, ""
		case "accepted":
			return connect.PaymentAccepted, ""
		default:
			return connect.PaymentUnknown, truncateReason(err.Error())
		}
	}
	return connect.PaymentUnknown, truncateReason(err.Error())
}

func truncateReason(s string) string {
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

func normalizeTxid(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// opConnectPaymentStatus 实现 payment.status：只读查询，不自动重付。
func (d *Daemon) opConnectPaymentStatus(ctx context.Context, auth connect.Auth, params map[string]any, origin string) (any, *kmapi.ErrorDetail) {
	rawID, ok := params["clientRequestId"]
	if !ok {
		return nil, errDetail(kmapi.ErrBadRequest, "clientRequestId 必填")
	}
	clientRequestID, ok := rawID.(string)
	if !ok || strings.TrimSpace(clientRequestID) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "clientRequestId 非法")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	network := info.BsvNetwork
	identityKey := connect.PaymentIdentityKey(network, origin, auth.WalletPublicKey, auth.AppID, auth.AppPublicKey)
	key := connect.PaymentRequestKey(identityKey, clientRequestID)
	rec, ok := d.connectMgr.Payments().Get(key)
	if !ok {
		return nil, errDetail(kmapi.ErrNotFound, "付款不存在")
	}
	// 身份隔离：记录必须属于当前授权身份，否则视为不存在。
	if rec.Origin != origin || !strings.EqualFold(rec.OwnerPublicKeyHex, auth.WalletPublicKey) || rec.Network != network || rec.AppID != auth.AppID || !strings.EqualFold(rec.AppPublicKey, auth.AppPublicKey) {
		return nil, errDetail(kmapi.ErrNotFound, "付款不存在")
	}
	return paymentResultFromRecord(rec), nil
}

// rejectPaymentByApproval 在审批被拒绝/取消时将付款置为未提交，避免悬挂 pending。
// 条件更新：仅 pending 可收敛，已终态（并发执行完成等）则不覆盖。存储失败时
// 记录保持原状（已回滚）并排队后台重试，但冻结必须释放——该审批已终态，
// 不会再有执行来广播，不静默跳过。
func (d *Daemon) rejectPaymentByApproval(approval connect.Approval, reason string) {
	params := approval.Params
	if params == nil {
		return
	}
	keyRaw, _ := params["paymentKey"].(string)
	if keyRaw == "" {
		return
	}
	rec, ok := d.connectMgr.Payments().Get(keyRaw)
	if !ok {
		return
	}
	if rec.Status != connect.PaymentPending {
		return
	}
	updated := rec
	updated.Status = connect.PaymentNotSigned
	updated.Reason = reason
	if ok, _, err := d.connectMgr.Payments().PutIfUnchanged(keyRaw, rec, updated); err != nil {
		d.enqueuePaymentWrite(keyRaw, paymentRetryOp{op: "put", base: rec, next: updated})
		if held, has := d.deleteHeld(keyRaw); has && held != nil {
			_ = held.wallet.Release(held.spend)
			held.release()
		}
		return
	} else if !ok {
		return
	}
	if held, has := d.deleteHeld(keyRaw); has && held != nil {
		// 释放冻结输入，不广播。
		_ = held.wallet.Release(held.spend)
		held.release()
	}
}

var _ = json.Marshal
var _ = fmt.Sprintf
