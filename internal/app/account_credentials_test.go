package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

func credentialTestState(value string) aistudio.StorageState {
	state := aistudio.StorageState{}
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		state.Cookies = append(state.Cookies, aistudio.StateCookie{
			Name: name, Value: value, Domain: ".google.com", Path: "/", Expires: -1, Secure: true,
		})
	}
	return state
}

// credentialTestAdmin 使用临时账户和空闲 Worker，验证过程中不连接 Google。
func credentialTestAdmin(t *testing.T) (*runtimeAdmin, *aistudio.Account) {
	t.Helper()
	store := aistudio.NewAccountStore(t.TempDir())
	cfg := aistudio.DefaultAccountConfig("user@example.com")
	cfg.AuthUser = 1
	cfg.Enabled = false
	cfg.Proxy = "http://127.0.0.1:1234"
	account, publication, err := store.Create(cfg, credentialTestState("old-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if err := publication.Release(); err != nil {
		t.Fatal(err)
	}
	account.Models = []aistudio.Model{{ID: "preserved-model"}}
	pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
	headers, err := newAccountHeaderProvider([]*aistudio.Account{account}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(headers.Close)
	registry := newRequestRegistry(t.Context())
	admin := &runtimeAdmin{
		pool: pool, store: store, requests: registry, headers: headers,
		workers: &accountWorkerManager{pool: pool, accounts: map[string]*accountWorker{
			account.ID: {id: account.ID, label: account.ID},
		}},
		proxies: aistudio.NewProxyPool(pool, nil),
		verifyAccount: func(_ context.Context, _ *aistudio.StorageState, proxy string, _ int) (chromeauth.Verification, error) {
			if proxy != cfg.Proxy {
				t.Fatalf("验证未使用原账户出口: %s", proxy)
			}
			return chromeauth.Verification{Email: account.ID}, nil
		},
	}
	return admin, account
}

// TestReplaceAccountCredentials 验证更新凭证保留账户环境、禁用状态和模型配置。
func TestReplaceAccountCredentials(t *testing.T) {
	admin, account := credentialTestAdmin(t)
	previous := account.Config
	result, err := admin.ImportAccountState(t.Context(), api.AccountStateInput{
		ReplaceAccountID: account.ID, Label: strings.ToUpper(account.ID),
		Proxy: "http://127.0.0.1:4321", Locale: "ja-JP", Timezone: "Asia/Tokyo",
		AuthUser: 0, StorageState: credentialTestState("new-credential"),
	})
	if err != nil {
		t.Fatal(err)
	}
	previous.AuthUser = 0
	if !reflect.DeepEqual(account.Config, previous) || len(account.Models) != 1 || account.Models[0].ID != "preserved-model" {
		t.Fatalf("更新改动了原账户配置: %+v", account.Config)
	}
	stored, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil || stored.Cookies[0].Value != "new-credential" {
		t.Fatalf("未保存新凭证: %v", err)
	}
	if result.ID != account.ID || result.Enabled || result.State != "disabled" || result.Auth.VerifiedAt == nil {
		t.Fatalf("更新状态不正确: %+v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil || strings.Contains(string(raw), "new-credential") {
		t.Fatal("账户摘要泄露了凭证")
	}
}

// TestCredentialIdentityMismatch 验证显式邮箱、认证来源和更新目标均不能伪造实际身份。
func TestCredentialIdentityMismatch(t *testing.T) {
	for _, kind := range []string{"label", "source", "target", "signed_out"} {
		t.Run(kind, func(t *testing.T) {
			admin, account := credentialTestAdmin(t)
			before, err := os.ReadFile(account.StoragePath)
			if err != nil {
				t.Fatal(err)
			}
			input := api.AccountStateInput{ReplaceAccountID: account.ID, StorageState: credentialTestState("new-credential")}
			switch kind {
			case "label":
				input.Label = "other@example.com"
			case "source":
				input.StorageState.SetAuthExtension(aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", Email: "other@example.com"}})
			case "target":
				admin.verifyAccount = func(context.Context, *aistudio.StorageState, string, int) (chromeauth.Verification, error) {
					return chromeauth.Verification{Email: "other@example.com"}, nil
				}
			case "signed_out":
				admin.verifyAccount = func(context.Context, *aistudio.StorageState, string, int) (chromeauth.Verification, error) {
					return chromeauth.Verification{}, chromeauth.ErrSignedOut
				}
			}
			verify := admin.verifyAccount
			calls := 0
			admin.verifyAccount = func(ctx context.Context, state *aistudio.StorageState, proxy string, user int) (chromeauth.Verification, error) {
				calls++
				return verify(ctx, state, proxy, user)
			}
			_, err = admin.ImportAccountState(t.Context(), input)
			var operation *adminOperationError
			if !errors.As(err, &operation) || operation.HTTPStatus() != http.StatusBadRequest || calls != 1 {
				t.Fatalf("未执行身份验证并拒绝错误身份: calls=%d err=%v", calls, err)
			}
			after, err := os.ReadFile(account.StoragePath)
			if err != nil || string(before) != string(after) {
				t.Fatal("验证失败覆盖了原凭证")
			}
		})
	}
}

// TestCredentialWriteFailure 验证凭证写入失败恢复会话序号并保留原文件。
func TestCredentialWriteFailure(t *testing.T) {
	admin, account := credentialTestAdmin(t)
	previous := account.Config
	storage := account.StoragePath
	before, err := os.ReadFile(storage)
	if err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(account.Directory, "occupied")
	if err := os.Mkdir(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	verify := admin.verifyAccount
	admin.verifyAccount = func(ctx context.Context, state *aistudio.StorageState, proxy string, user int) (chromeauth.Verification, error) {
		result, err := verify(ctx, state, proxy, user)
		// 取得账户租约并验证身份后再模拟存储故障，确保覆盖实际写入路径。
		account.StoragePath = occupied
		return result, err
	}
	_, err = admin.ImportAccountState(t.Context(), api.AccountStateInput{
		ReplaceAccountID: account.ID, AuthUser: 0, StorageState: credentialTestState("new-credential"),
	})
	if err == nil || !strings.Contains(err.Error(), "替换文件") {
		t.Fatalf("未执行凭证写入失败路径: %v", err)
	}
	account.StoragePath = storage
	after, err := os.ReadFile(storage)
	if err != nil || string(before) != string(after) || !reflect.DeepEqual(account.Config, previous) {
		t.Fatal("写入失败未保留原凭证和配置")
	}
	configBytes, err := os.ReadFile(account.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored aistudio.AccountConfig
	if json.Unmarshal(configBytes, &stored) != nil || stored.AuthUser != previous.AuthUser {
		t.Fatal("未恢复磁盘中的会话序号")
	}
}

// TestCredentialImportStillRequiresExplicitReplacement 验证普通重复导入不覆盖账户。
func TestCredentialImportStillRequiresExplicitReplacement(t *testing.T) {
	admin, account := credentialTestAdmin(t)
	_, err := admin.ImportAccountState(t.Context(), api.AccountStateInput{
		Label: account.ID, Proxy: account.Config.Proxy, StorageState: credentialTestState("new-credential"),
	})
	var operation *adminOperationError
	if !errors.As(err, &operation) || operation.HTTPStatus() != http.StatusConflict {
		t.Fatalf("普通重复导入 = %v", err)
	}
}

// TestAuthRefreshUnsupported 验证非支持平台不会因存在 OAuth 材料而尝试自动续签。
func TestAuthRefreshUnsupported(t *testing.T) {
	admin, account := credentialTestAdmin(t)
	state := credentialTestState("old-credential")
	state.SetAuthExtension(aistudio.AuthExtension{
		Source: aistudio.AuthSource{Browser: "chrome", Email: account.ID},
		OAuth:  &aistudio.ChromeOAuthMaterial{GaiaID: "test-gaia", RefreshToken: "private-refresh-token"},
	})
	if err := aistudio.WriteStorageState(account.StoragePath, state); err != nil {
		t.Fatal(err)
	}
	lease, err := admin.pool.AcquireAccount(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	refresher := &authRuntimeRefresher{supported: func() bool { return false }}
	if refresher.Available(aistudio.ContextWithAccountLease(t.Context(), lease)) {
		t.Fatal("不支持的平台仍被判定为可续签")
	}
}

// TestAuthRefreshFailureStatus 验证失败续签保留原凭证且账户摘要不包含错误或令牌正文。
func TestAuthRefreshFailureStatus(t *testing.T) {
	admin, account := credentialTestAdmin(t)
	state := credentialTestState("old-credential")
	state.SetAuthExtension(aistudio.AuthExtension{
		Source: aistudio.AuthSource{Browser: "chrome", Email: account.ID},
		OAuth:  &aistudio.ChromeOAuthMaterial{GaiaID: "test-gaia", RefreshToken: "private-refresh-token"},
	})
	if err := aistudio.WriteStorageState(account.StoragePath, state); err != nil {
		t.Fatal(err)
	}
	lease, err := admin.pool.AcquireAccount(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	refresher := &authRuntimeRefresher{
		pool: admin.pool, requests: admin.requests,
		refresh: func(context.Context, aistudio.AuthSource, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
			return nil, errors.New("test-upstream-error")
		},
	}
	ctx := aistudio.ContextWithAccountLease(t.Context(), lease)
	if err := refresher.Refresh(ctx, lease.AuthGeneration()); err == nil {
		t.Fatal("续签失败被当作成功")
	}
	status := adminAccountDTO(admin.pool.Status()[0])
	if status.Auth.LastRefreshAt == nil || status.Auth.LastRefreshResult != "failed" {
		t.Fatalf("未记录续签失败: %+v", status.Auth)
	}
	raw, err := json.Marshal(status)
	if err != nil || strings.Contains(string(raw), "private-refresh-token") || strings.Contains(string(raw), "test-upstream-error") {
		t.Fatal("认证摘要包含了敏感材料或错误正文")
	}
	stored, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil || stored.Cookies[0].Value != "old-credential" {
		t.Fatal("失败的续签覆盖了原凭证")
	}
}

// TestRecordedVerificationPreservesAuthenticationFailure 验证记录导入验收时间不会恢复已失效账户。
func TestRecordedVerificationPreservesAuthenticationFailure(t *testing.T) {
	admin, account := credentialTestAdmin(t)
	lease, err := admin.pool.AcquireAccount(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	cfg := account.Config
	cfg.Enabled = true
	if err := lease.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := lease.ConfirmAuthenticationFailure(t.Context(), camoufoxnative.ErrAuthenticationRequired); err != nil {
		t.Fatal(err)
	}
	checkedAt := lease.CheckedAt()
	if err := admin.pool.RecordAuthenticationVerified(account.ID, checkedAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	status := admin.pool.Status()[0]
	if status.State != aistudio.AccountAuthRequired || status.Auth.VerifiedAt != nil {
		t.Fatal("过期的导入验证覆盖了后续认证失败")
	}
	if err := admin.pool.RecordAuthenticationVerified(account.ID, checkedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	status = admin.pool.Status()[0]
	if status.State != aistudio.AccountAuthRequired || status.Auth.VerifiedAt == nil {
		t.Fatal("记录验证时间意外改变了账户调度状态")
	}
}
