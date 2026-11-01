package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type accountQuotaTransport func(context.Context, aistudio.RPCRequest) (*aistudio.RPCResponse, error)

func (transport accountQuotaTransport) Do(ctx context.Context, request aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return transport(ctx, request)
}

func TestAccountQuotaPreservesState(t *testing.T) {
	admin, account := credentialTestAdmin(t)
	previousConfig := account.Config
	previousCredentials, err := os.ReadFile(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	generation := admin.pool.ModelAccessGeneration(account.ID)
	for index, scope := range []string{"*", "preserved-model", "build:preserved-model"} {
		source := "upstream"
		if index == 1 {
			source = "estimated"
		}
		_, err := admin.pool.MarkQuotaCooldownIfGeneration(account.ID, scope, generation, time.Now(), aistudio.QuotaCooldown{
			Until: time.Now().Add(time.Hour), Kind: "每日限额", Reason: "每日限额: fixture-sensitive-error", ResetSource: source,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	before := admin.pool.Status()[0].Cooldowns
	admin.quotaClient = aistudio.NewClient(aistudio.ClientOptions{Transport: accountQuotaTransport(func(ctx context.Context, request aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
		lease, ok := aistudio.AccountLeaseFromContext(ctx)
		if !ok || lease.Account().ID != account.ID || request.AccountID != account.ID || request.Method != "GetModelQuota" {
			t.Fatal("查询未绑定目标账户租约")
		}
		return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": {aistudio.JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader(`[[["successful_video_generations",50,0]],{"sensitive":"fixture-secret"}]`))}, nil
	})})
	report, err := admin.AccountQuota(t.Context(), account.ID, "preserved-model")
	if err != nil || report.Status != "available" || len(report.Metrics) != 1 || *report.Metrics[0].Remaining != 0 || len(report.Recoveries) != 2 {
		t.Fatalf("额度快照不正确: %+v, %v", report, err)
	}
	if report.Recoveries[0].Source != "upstream" || report.Recoveries[1].Source != "estimated" || report.Recoveries[1].Scope != "preserved-model" {
		t.Fatalf("恢复时间来源不正确: %+v", report.Recoveries)
	}
	afterCredentials, err := os.ReadFile(account.StoragePath)
	if err != nil || string(afterCredentials) != string(previousCredentials) || !reflect.DeepEqual(account.Config, previousConfig) || !reflect.DeepEqual(before, admin.pool.Status()[0].Cooldowns) {
		t.Fatal("额度查询改动了凭证、账户配置或冷却")
	}
	if account.Config.Enabled || len(account.Models) != 1 {
		t.Fatal("查询改变了禁用状态或模型目录")
	}
	raw, err := json.Marshal(report)
	if err != nil || strings.Contains(string(raw), "fixture-secret") || strings.Contains(string(raw), "fixture-sensitive-error") || strings.Contains(string(raw), "old-credential") {
		t.Fatal("额度响应包含敏感或非业务字段")
	}
	lease, err := admin.pool.AcquireAccount(t.Context(), account.ID)
	if err != nil {
		t.Fatalf("查询没有释放账户租约: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	// 运行状态经磁盘重新载入后仍保留恢复时间来源。
	loaded, err := admin.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := aistudio.NewAccountPool(loaded, 1).Status()[0]
	if snapshot.Cooldowns["*"].ResetSource != "upstream" || snapshot.Cooldowns["preserved-model"].ResetSource != "estimated" {
		t.Fatal("恢复时间来源没有持久化")
	}
}

func TestAccountQuotaFailures(t *testing.T) {
	for _, item := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"empty", 200, `[]`, "unknown"},
		{"unsupported", 404, `[]`, "unavailable"},
		{"not_implemented", 501, `[]`, "unavailable"},
		{"upstream_failure", 503, `{"error":{"message":"fixture-secret"}}`, "error"},
		{"invalid_protocol", 200, `[[{"secret":"fixture-secret"}]]`, "error"},
	} {
		t.Run(item.name, func(t *testing.T) {
			admin, account := credentialTestAdmin(t)
			calls := 0
			admin.quotaClient = aistudio.NewClient(aistudio.ClientOptions{Transport: accountQuotaTransport(func(context.Context, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
				calls++
				return &aistudio.RPCResponse{StatusCode: item.status, Header: http.Header{"Content-Type": {aistudio.JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader(item.body))}, nil
			})})
			report, err := admin.AccountQuota(t.Context(), account.ID, "preserved-model")
			if item.want == "error" {
				var operation *adminOperationError
				if !errors.As(err, &operation) || operation.HTTPStatus() != http.StatusBadGateway || errors.Unwrap(operation) == nil || strings.Contains(err.Error(), "fixture-secret") {
					t.Fatalf("未安全保留查询错误: %v", err)
				}
			} else if err != nil || report.Status != item.want || report.Metrics == nil || len(report.Metrics) != 0 {
				t.Fatalf("空结果/不支持被显示为可用: %+v %v", report, err)
			}
			if calls != 1 || account.Config.Enabled {
				t.Fatal("查询次数或账户状态不正确")
			}
			if _, err := admin.AccountQuota(t.Context(), account.ID, "another-model"); err == nil || calls != 1 {
				t.Fatal("目录外模型仍向上游查询")
			}
			if _, err := admin.AccountQuota(t.Context(), "missing@example.com", "preserved-model"); err == nil || calls != 1 {
				t.Fatal("不存在的账户仍向上游查询")
			}
		})
	}
}

func TestQuotaRecoveryDoesNotInventSource(t *testing.T) {
	for _, item := range []struct {
		reason string
		quota  bool
	}{
		{"每日限额: legacy", true}, {"套餐限额: legacy", true}, {"Build 拒绝: legacy", false}, {"network error", false},
	} {
		if isQuotaCooldown(aistudio.CooldownState{Reason: item.reason}) != item.quota {
			t.Errorf("冷却识别不正确: %s", item.reason)
		}
	}
}
