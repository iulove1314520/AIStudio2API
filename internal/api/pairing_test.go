package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pairingAdmin 记录配对上传的账户
type pairingAdmin struct {
	AdminService
	imported []AccountStateInput
}

func (admin *pairingAdmin) Accounts(context.Context) ([]AdminAccount, error) {
	return []AdminAccount{{ID: "a@example.com", Label: "a@example.com"}}, nil
}

// ImportAccountState 保存上传内容并返回同名账户
func (admin *pairingAdmin) ImportAccountState(_ context.Context, input AccountStateInput) (AdminAccount, error) {
	admin.imported = append(admin.imported, input)
	return AdminAccount{ID: input.Label, Label: input.Label}, nil
}

// TestPairingTokenAccount 验证令牌由管理会话签发、上传不需要管理会话、新令牌使旧令牌失效
func TestPairingTokenAccount(t *testing.T) {
	admin := &pairingAdmin{}
	handler := NewHandler(nil, Config{Admin: admin, AdminAuthEnabled: true, AdminUsername: "admin", AdminPassword: "secret"})
	serve := func(method, path, body string, header http.Header) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		for key, values := range header {
			request.Header[key] = values
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if response := serve("POST", "/api/pairing", "", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("无会话签发令牌 = %d", response.Code)
	}
	login := serve("POST", "/api/auth/login", `{"username":"admin","password":"secret"}`, nil)
	cookies := login.Result().Cookies()
	if login.Code != http.StatusOK || len(cookies) == 0 {
		t.Fatalf("登录 = %d %s", login.Code, login.Body)
	}
	session := http.Header{"Cookie": {cookies[0].String()}}
	issue := func() string {
		response := serve("POST", "/api/pairing", "", session)
		var token PairingToken
		if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &token) != nil || token.Token == "" {
			t.Fatalf("签发令牌 = %d %s", response.Code, response.Body)
		}
		if remaining := time.Until(token.ExpiresAt); remaining <= 9*time.Minute || remaining > 10*time.Minute {
			t.Fatalf("令牌有效期 = %s", remaining)
		}
		return token.Token
	}
	upload := func(token string) int {
		header := http.Header{"Content-Type": {"application/json"}}
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
		body := `{"label":"a@example.com","locale":"en-US","timezone":"UTC","storage_state":{"cookies":[],"origins":[]},"fingerprint":{"config":{"a":1}}}`
		return serve("POST", "/api/pairing/accounts", body, header).Code
	}
	first := issue()
	if code := upload(""); code != http.StatusUnauthorized {
		t.Fatalf("无令牌上传 = %d", code)
	}
	if code := upload(first + "x"); code != http.StatusUnauthorized {
		t.Fatalf("错误令牌上传 = %d", code)
	}
	if code := upload(first); code != http.StatusCreated {
		t.Fatalf("令牌上传 = %d", code)
	}
	if code := upload(first); code != http.StatusCreated {
		t.Fatalf("有效期内第二个账户 = %d", code)
	}
	second := issue()
	if code := upload(first); code != http.StatusUnauthorized {
		t.Fatalf("旧令牌上传 = %d", code)
	}
	if code := upload(second); code != http.StatusCreated {
		t.Fatalf("新令牌上传 = %d", code)
	}
	if len(admin.imported) != 3 || admin.imported[0].Label != "a@example.com" || string(admin.imported[0].Fingerprint) != `{"config":{"a":1}}` {
		t.Fatalf("导入内容 = %+v", admin.imported)
	}
}

// TestPairingTokenExpiry 验证令牌到期后失效
func TestPairingTokenExpiry(t *testing.T) {
	tokens := &pairingTokens{}
	now := time.Unix(1_700_000_000, 0)
	token := tokens.issue(now)
	if !tokens.valid(token.Token, now.Add(pairingTokenLifetime-time.Second)) {
		t.Fatal("到期前令牌无效")
	}
	if tokens.valid(token.Token, now.Add(pairingTokenLifetime)) {
		t.Fatal("到期后令牌仍有效")
	}
	if (&pairingTokens{}).valid("", now) {
		t.Fatal("未签发时空令牌有效")
	}
}

// TestPairingUpdateScope 验证普通令牌不能覆盖账户，更新令牌只允许更新选定账户。
func TestPairingUpdateScope(t *testing.T) {
	admin := &pairingAdmin{}
	s := &server{config: Config{Admin: admin}, pairing: &pairingTokens{}}
	issue := func(target string, status int) PairingToken {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"replace_account_id": target})
		r := httptest.NewRequest("POST", "/api/pairing", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		s.handleCreatePairing(w, r)
		if w.Code != status {
			t.Fatalf("签发更新令牌 = %d %s", w.Code, w.Body)
		}
		var token PairingToken
		if status == http.StatusCreated && json.Unmarshal(w.Body.Bytes(), &token) != nil {
			t.Fatal("令牌格式不正确")
		}
		return token
	}
	upload := func(token, target string) int {
		body, _ := json.Marshal(AccountStateInput{Label: "a@example.com", ReplaceAccountID: target})
		r := httptest.NewRequest("POST", "/api/pairing/accounts", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.handlePairingAccount(w, r)
		return w.Code
	}
	issue("missing@example.com", http.StatusNotFound)
	ordinary := issue("", http.StatusCreated)
	if code := upload(ordinary.Token, "a@example.com"); code != http.StatusForbidden {
		t.Fatalf("普通令牌可覆盖账户: %d", code)
	}
	bound := issue("a@example.com", http.StatusCreated)
	if bound.ReplaceAccountID != "a@example.com" {
		t.Fatal("令牌未返回更新目标")
	}
	if code := upload(bound.Token, "other@example.com"); code != http.StatusForbidden {
		t.Fatalf("绑定令牌可覆盖其他账户: %d", code)
	}
	if code := upload(bound.Token, ""); code != http.StatusCreated {
		t.Fatalf("旧客户端定向更新 = %d", code)
	}
	if len(admin.imported) != 1 || admin.imported[0].ReplaceAccountID != "a@example.com" {
		t.Fatal("服务端未强制使用令牌绑定的更新目标")
	}
}
