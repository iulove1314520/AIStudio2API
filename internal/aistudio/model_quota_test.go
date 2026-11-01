package aistudio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type quotaTestTransport func(context.Context, RPCRequest) (*RPCResponse, error)

func (transport quotaTestTransport) Do(ctx context.Context, request RPCRequest) (*RPCResponse, error) {
	return transport(ctx, request)
}

// TestModelQuotaProtocol 使用人工协议样本区分真实零值、未返回值与空结果。
func TestModelQuotaProtocol(t *testing.T) {
	for _, raw := range []string{`[]`, `[null]`, `[[]]`} {
		metrics, err := decodeModelQuota([]byte(raw))
		if err != nil || metrics == nil || len(metrics) != 0 {
			t.Fatalf("空结果 = %+v, %v", metrics, err)
		}
	}
	metrics, err := decodeModelQuota([]byte(`[[["successful_video_generations","50",0,{"unconfirmed_reset":"ignored"}],["future_metric",null,"12"],["missing_values"]],"unknown_root_field"]`))
	if err != nil || len(metrics) != 3 {
		t.Fatalf("解析指标 = %+v, %v", metrics, err)
	}
	if metrics[0].Limit == nil || *metrics[0].Limit != 50 || metrics[0].Remaining == nil || *metrics[0].Remaining != 0 {
		t.Fatalf("真实零余量丢失 = %+v", metrics[0])
	}
	if metrics[1].Limit != nil || metrics[1].Remaining == nil || *metrics[1].Remaining != 12 || metrics[2].Limit != nil || metrics[2].Remaining != nil {
		t.Fatal("缺失字段被当作零")
	}
	for _, raw := range []string{`{}`, `[{}]`, `[[null]]`, `[[[]]]`, `[[["",1,1]]]`, `[[["metric",-1,1]]]`, `[[["metric",1,{}]]]`, `[[["metric",1.5,1]]]`, `[[["metric","9223372036854775808",1]]]`} {
		t.Run(raw, func(t *testing.T) {
			_, err := decodeModelQuota([]byte(raw))
			var evidence *ProtocolEvidenceError
			if !errors.As(err, &evidence) || evidence.Method != "GetModelQuota" {
				t.Fatalf("异常协议没有严格拒绝: %v", err)
			}
		})
	}
}

func TestModelQuotaRequest(t *testing.T) {
	calls := 0
	client := NewClient(ClientOptions{Transport: quotaTestTransport(func(ctx context.Context, request RPCRequest) (*RPCResponse, error) {
		calls++
		if request.Method != "GetModelQuota" || request.URL != MakerSuiteRPCBase+"GetModelQuota" || request.AccountID != "a@example.com" || string(request.Body) != `["models/veo-test"]` || request.Streaming {
			t.Fatalf("额度请求不正确: %+v", request)
		}
		return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader(`[[["successful_video_generations",5,3]]]`))}, nil
	})})
	metrics, err := client.ModelQuotaForAccount(t.Context(), "a@example.com", " models/veo-test ")
	if err != nil || len(metrics) != 1 || *metrics[0].Remaining != 3 || calls != 1 {
		t.Fatalf("查询失败: %+v %v", metrics, err)
	}
	for _, model := range []string{"", "models/", "../model", "test/path", "test?key=secret", strings.Repeat("x", 201)} {
		if _, err := client.ModelQuotaForAccount(t.Context(), "a@example.com", model); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("无效模型未拒绝: %q %v", model, err)
		}
	}
	if calls != 1 {
		t.Fatal("无效模型仍发送到上游")
	}
}

func TestQuotaRecoverySource(t *testing.T) {
	now := time.Now()
	for _, item := range []struct {
		name, message, source string
		delay                 time.Duration
	}{
		{"retry_hint", "daily limit", "upstream", 10 * time.Minute},
		{"reset_message", "You've reached your usage limit (resets in 2 hours, 3 minutes)", "upstream", 0},
		{"daily_estimate", "daily limit", "estimated", 0},
		{"generic_backoff", "You've reached your usage limit", "estimated", 0},
	} {
		t.Run(item.name, func(t *testing.T) {
			cooldown, ok := QuotaCooldownForError(&RPCError{StatusCode: 429, Message: item.message, RetryDelay: item.delay}, now)
			if !ok || cooldown.ResetSource != item.source || quotaKindCode(cooldown) == "" {
				t.Fatalf("恢复来源不正确: %+v", cooldown)
			}
			if item.delay > 0 && !cooldown.Until.Equal(now.Add(item.delay)) {
				t.Fatal("未保留上游重试期限")
			}
		})
	}
	if quotaKindCode(QuotaCooldown{Kind: "Build 拒绝", Escalate: true}) != "" {
		t.Fatal("Build 参数拒绝被标记为额度")
	}
}

func TestQuotaCooldownRetainsActiveSource(t *testing.T) {
	account := &Account{ID: "a@example.com", Config: AccountConfig{Enabled: true}, State: AccountReady, modelAccessGeneration: 1}
	pool := NewAccountPool([]*Account{account}, 1)
	generation := pool.ModelAccessGeneration(account.ID)
	first := QuotaCooldown{Kind: "套餐限额", Reason: "套餐限额: fixture", Escalate: true, ResetSource: "estimated"}
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, "test-model", generation, time.Now(), first); err != nil {
		t.Fatal(err)
	}
	before := pool.Status()[0].Cooldowns["test-model"]
	if before.Level != 1 || before.ResetSource != "estimated" || before.QuotaKind != "plan" {
		t.Fatalf("首次递增冷却元数据不正确: %+v", before)
	}
	// 并发失败沿用未结束的旧期限，也必须沿用旧期限的来源。
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, "test-model", generation, time.Now(), QuotaCooldown{Kind: "Build 拒绝", Escalate: true}); err != nil {
		t.Fatal(err)
	}
	if got := pool.Status()[0].Cooldowns["test-model"]; got != before {
		t.Fatalf("沿用旧期限却更改了来源: %+v", got)
	}
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, "test-model", generation+1, time.Now(), first); err != nil {
		t.Fatal(err)
	}
	if got := pool.Status()[0].Cooldowns["test-model"]; got != before {
		t.Fatal("旧目录版本写入了冷却元数据")
	}
}
