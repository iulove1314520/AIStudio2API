package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type quotaAdmin struct {
	AdminService
	calls int
}

func (admin *quotaAdmin) AccountQuota(ctx context.Context, account, model string) (AdminAccountQuota, error) {
	admin.calls++
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 30*time.Second || account != "a@example.com" || model != "veo-test" {
		panic("额度请求参数或超时不正确")
	}
	zero := int64(0)
	return AdminAccountQuota{AccountID: account, ModelID: model, Status: "available", Metrics: []aistudio.ModelQuotaMetric{{Name: "successful_video_generations", Remaining: &zero}}}, nil
}

func TestAccountQuotaAuthenticationAndValidation(t *testing.T) {
	admin := &quotaAdmin{}
	handler := NewHandler(nil, Config{Admin: admin, AdminAuthEnabled: true, AdminUsername: "admin", AdminPassword: "secret"})
	request := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := "/api/accounts/a%40example.com/quota?model=models%2Fveo-test"
	if response := request(path, nil); response.Code != http.StatusUnauthorized || admin.calls != 0 {
		t.Fatal("未登录用户可以读取额度")
	}
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, login)
	if w.Code != http.StatusOK || len(w.Result().Cookies()) == 0 {
		t.Fatal("测试登录失败")
	}
	cookie := w.Result().Cookies()[0]
	for _, query := range []string{"", "?model=", "?model=../test", "?model=a%2Fb"} {
		if response := request("/api/accounts/a%40example.com/quota"+query, cookie); response.Code != http.StatusBadRequest || admin.calls != 0 {
			t.Fatalf("无效查询没有拒绝: %s %d", query, response.Code)
		}
	}
	response := request(path, cookie)
	if response.Code != http.StatusOK || admin.calls != 1 || !strings.Contains(response.Body.String(), `"remaining":0`) || !strings.Contains(response.Body.String(), `"limit":null`) {
		t.Fatalf("额度接口零值/未知字段不正确: %d %s", response.Code, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("额度接口允许缓存管理数据")
	}
}
