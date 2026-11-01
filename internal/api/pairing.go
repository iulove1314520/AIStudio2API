package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// pairingTokenLifetime 是远程配对令牌的有效期
const pairingTokenLifetime = 10 * time.Minute

// PairingToken 是远程配对令牌与到期时间
type PairingToken struct {
	Token            string    `json:"token"`
	ExpiresAt        time.Time `json:"expires_at"`
	ReplaceAccountID string    `json:"replace_account_id,omitempty"`
}

// AccountStateInput 是远程配对上传或管理端 Cookie 导入的账户认证状态与 Camoufox 指纹
type AccountStateInput struct {
	ReplaceAccountID string                `json:"replace_account_id,omitempty"`
	Label            string                `json:"label"`
	Proxy            string                `json:"-"`
	Locale           string                `json:"locale"`
	Timezone         string                `json:"timezone"`
	AuthUser         int                   `json:"auth_user"`
	StorageState     aistudio.StorageState `json:"storage_state"`
	Fingerprint      json.RawMessage       `json:"fingerprint,omitempty"`
}

// pairingTokens 保存当前唯一有效配对令牌的摘要
type pairingTokens struct {
	mu               sync.Mutex
	digest           [sha256.Size]byte
	expires          time.Time
	replaceAccountID string
}

// issue 生成新令牌并使旧令牌失效
func (tokens *pairingTokens) issue(now time.Time) PairingToken {
	return tokens.issueFor(now, "")
}

// issueFor 将凭证更新令牌限定到管理员选择的单个账户。
func (tokens *pairingTokens) issueFor(now time.Time, accountID string) PairingToken {
	token := rand.Text()
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	tokens.digest = sha256.Sum256([]byte(token))
	tokens.expires = now.Add(pairingTokenLifetime)
	tokens.replaceAccountID = accountID
	return PairingToken{Token: token, ExpiresAt: tokens.expires, ReplaceAccountID: accountID}
}

// valid 判断令牌是否为当前未过期的配对令牌
func (tokens *pairingTokens) valid(token string, now time.Time) bool {
	_, valid := tokens.authorize(token, now)
	return valid
}

func (tokens *pairingTokens) authorize(token string, now time.Time) (string, bool) {
	digest := sha256.Sum256([]byte(token))
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	valid := now.Before(tokens.expires) && subtle.ConstantTimeCompare(digest[:], tokens.digest[:]) == 1
	return tokens.replaceAccountID, valid
}

// handleCreatePairing 生成远程配对令牌
func (s *server) handleCreatePairing(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ReplaceAccountID string `json:"replace_account_id"`
	}
	if err := decodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Invalid pairing request")
		return
	}
	target := strings.ToLower(strings.TrimSpace(input.ReplaceAccountID))
	if target != "" {
		accounts, err := s.config.Admin.Accounts(r.Context())
		if err != nil {
			writeAdminUpstreamError(w, err)
			return
		}
		found := false
		for _, account := range accounts {
			found = found || account.ID == target
		}
		if !found {
			writeAdminError(w, http.StatusNotFound, "account_not_found", "Account not found")
			return
		}
	}
	writeJSON(w, http.StatusCreated, s.pairing.issueFor(time.Now(), target))
}

// handlePairingAccount 凭配对令牌导入一个账户，不经过管理会话
func (s *server) handlePairingAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	target, valid := s.pairing.authorize(strings.TrimSpace(token), time.Now())
	if !valid {
		writeAdminError(w, http.StatusUnauthorized, "pairing_token_invalid", "Pairing token is invalid or expired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var input AccountStateInput
	if err := decodeJSON(r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Invalid account state")
		return
	}
	if requested := strings.ToLower(strings.TrimSpace(input.ReplaceAccountID)); requested != "" && requested != target {
		writeAdminError(w, http.StatusForbidden, "pairing_scope_mismatch", "Pairing token cannot update this account")
		return
	}
	input.ReplaceAccountID = target
	account, err := s.config.Admin.ImportAccountState(r.Context(), input)
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]AdminAccount{"account": account})
}
