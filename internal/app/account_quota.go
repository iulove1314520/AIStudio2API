package app

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// AccountQuota 使用独占管理租约读取单个账户的真实官网额度，不生成内容或解除冷却。
func (admin *runtimeAdmin) AccountQuota(ctx context.Context, accountID, model string) (api.AdminAccountQuota, error) {
	model, err := aistudio.QuotaModelID(model)
	if err != nil {
		return api.AdminAccountQuota{}, invalidAccount(err)
	}
	account, err := admin.account(accountID)
	if err != nil {
		return api.AdminAccountQuota{}, err
	}
	if !slices.Contains(account.Models, model) {
		return api.AdminAccountQuota{}, &adminOperationError{status: http.StatusBadRequest, code: "invalid_request", message: "请先刷新账户目录，并选择该账户的模型"}
	}
	if admin.quotaClient == nil {
		return api.AdminAccountQuota{}, &adminOperationError{status: http.StatusServiceUnavailable, code: "account_quota_unavailable", message: "无法读取 Google 额度"}
	}
	lease, err := admin.pool.AcquireAccount(ctx, accountID)
	if err != nil {
		return api.AdminAccountQuota{}, accountOperationError(err)
	}
	metrics, readErr := admin.quotaClient.ModelQuotaForAccount(aistudio.ContextWithAccountLease(ctx, lease), accountID, model)
	if releaseErr := lease.Release(); releaseErr != nil {
		return api.AdminAccountQuota{}, errors.Join(readErr, releaseErr)
	}
	status := "available"
	if readErr != nil {
		var rpcError *aistudio.RPCError
		if errors.As(readErr, &rpcError) && (rpcError.StatusCode == http.StatusNotFound || rpcError.StatusCode == http.StatusNotImplemented) {
			status = "unavailable"
			metrics = []aistudio.ModelQuotaMetric{}
		} else {
			return api.AdminAccountQuota{}, &adminOperationError{status: http.StatusBadGateway, code: "account_quota_unavailable", message: "Google 额度查询失败，请稍后重试或检查账户登录状态", cause: readErr}
		}
	} else if len(metrics) == 0 {
		status = "unknown"
	}
	checkedAt := time.Now().UTC()
	recoveries := make([]api.QuotaRecovery, 0)
	for _, snapshot := range admin.pool.Status() {
		if snapshot.ID != accountID {
			continue
		}
		// GetModelQuota 对应官网 Playground，Build 的同名模型冷却不混入。
		for _, scope := range []string{"*", model} {
			cooldown, ok := snapshot.Cooldowns[scope]
			if !ok || !cooldown.Active(checkedAt) || !isQuotaCooldown(cooldown) {
				continue
			}
			source := cooldown.ResetSource
			if source != "upstream" && source != "estimated" {
				source = "unknown"
			}
			recoveries = append(recoveries, api.QuotaRecovery{Scope: scope, Until: cooldown.Until, Source: source, Kind: cooldown.QuotaKind})
		}
	}
	return api.AdminAccountQuota{
		AccountID: accountID, ModelID: model, Channel: string(aistudio.ChannelPlayground), Status: status,
		CheckedAt: checkedAt, Metrics: metrics, Recoveries: recoveries,
	}, nil
}

func isQuotaCooldown(cooldown aistudio.CooldownState) bool {
	if cooldown.QuotaKind != "" {
		return true
	}
	// 旧运行数据没有来源字段，只保留可以辨认的限额记录并标记来源未知。
	for _, kind := range []string{"每日限额", "分钟限额", "套餐限额", "无配额", "短期限额"} {
		if strings.HasPrefix(cooldown.Reason, kind+": ") {
			return true
		}
	}
	return false
}
