package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// 管理后台鉴权：密码登录（session cookie）+ 兼容旧 token（Bearer / ?token=）。
//
// 密码 hash 格式：v1$<saltHex>$<hex(sha256(salt+password))>，salt 16 字节随机，
// 存在 settings 表的 admin.password_hash。首次使用时经 /nb/admin/api/login
// 用 new_password 设置。

const (
	// passwordHashSettingKey 是管理密码 hash 在 settings 表中的 key。
	passwordHashSettingKey = "admin.password_hash"
	// sessionCookieName 是登录 session 的 cookie 名。
	sessionCookieName = "nb_session"
	// sessionTTL 是 session 有效期。
	sessionTTL = 24 * time.Hour
)

// hashPassword 生成 v1$<saltHex>$<hex(sha256(salt+password))>。
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pw))
	return "v1$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(h.Sum(nil)), nil
}

// checkPassword 校验密码（常量时间比较防时序攻击）。
func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	salt, err1 := hex.DecodeString(parts[1])
	want, err2 := hex.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(salt) != 16 || len(want) != sha256.Size {
		return false
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pw))
	return subtle.ConstantTimeCompare(h.Sum(nil), want) == 1
}

// ---------------------------------------------------------------- session

// createSession 建一个 24h 有效的 session，返回 token。
func (s *Server) createSession() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	now := time.Now()
	s.webUI.sessMu.Lock()
	defer s.webUI.sessMu.Unlock()
	s.webUI.sessions[tok] = now.Add(sessionTTL)
	// 惰性清理过期 session。
	for k, exp := range s.webUI.sessions {
		if now.After(exp) {
			delete(s.webUI.sessions, k)
		}
	}
	return tok, nil
}

// validSession 校验请求 cookie 中的 session。
func (s *Server) validSession(r *http.Request) bool {
	if s.webUI == nil {
		return false
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	s.webUI.sessMu.Lock()
	defer s.webUI.sessMu.Unlock()
	exp, ok := s.webUI.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.webUI.sessions, c.Value)
		return false
	}
	return true
}

// destroySession 删除请求 cookie 对应的 session。
func (s *Server) destroySession(r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return
	}
	s.webUI.sessMu.Lock()
	delete(s.webUI.sessions, c.Value)
	s.webUI.sessMu.Unlock()
}

func setSessionCookie(w http.ResponseWriter, token string, clear bool) {
	c := &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/nb/admin/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	}
	if clear {
		c.Value = ""
		c.MaxAge = -1
		c.Expires = time.Unix(0, 0)
	}
	http.SetCookie(w, c)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- handler

// handleLogin 管理员登录/首次设密/改密（不鉴权）。
//
//   - 首次：settings 无 admin.password_hash 且 body 带 new_password → 设置密码并登录
//   - 登录：body 带 password 且 hash 校验通过 → 建 session
//   - 改密：已登录 + {change:true}，新密码取 new_password（无则取 password，兼容前端设置页）→ 更新 hash
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.db == nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "store_not_attached"})
		return
	}
	var body struct {
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
		Change      bool   `json:"change"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	ctx := r.Context()
	hash, hasHash, err := s.db.GetSetting(ctx, passwordHashSettingKey)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	issue := func() {
		tok, err := s.createSession()
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		setSessionCookie(w, tok, false)
		writeJSON(w, map[string]any{"ok": true})
	}

	// 首次设置密码。
	if !hasHash {
		if body.NewPassword == "" {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "password_not_set"})
			return
		}
		h, err := hashPassword(body.NewPassword)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := s.db.SetSetting(ctx, passwordHashSettingKey, h); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.logger.Printf("admin 首次设置管理密码")
		issue()
		return
	}
	// 改密码（需已登录）。新密码优先取 new_password；前端设置页只发
	// {password: 新密码, change:true}，此时 password 即新密码。
	if body.Change {
		if !s.validSession(r) {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		newPW := body.NewPassword
		if newPW == "" {
			newPW = body.Password
		}
		if newPW == "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "new_password_required"})
			return
		}
		h, err := hashPassword(newPW)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := s.db.SetSetting(ctx, passwordHashSettingKey, h); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.logger.Printf("admin 修改管理密码")
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	// 登录。
	if body.Password != "" && checkPassword(hash, body.Password) {
		issue()
		return
	}
	writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "invalid_password"})
}

// handleLoginStatus 返回是否已设置管理密码（不鉴权，供前端判断显示"设置密码"还是"登录"）。
func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	set := false
	if s.db != nil {
		_, set, _ = s.db.GetSetting(r.Context(), passwordHashSettingKey)
	}
	writeJSON(w, map[string]any{"passwordSet": set})
}

// handleLogout 退出登录：删 session、清 cookie。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.webUI != nil {
		s.destroySession(r)
	}
	setSessionCookie(w, "", true)
	writeJSON(w, map[string]any{"ok": true})
}

// requireAdmin 是管理后台鉴权中间件：session 有效，或原有 Bearer / ?token=
// （NB_ADMIN_TOKEN）二选一；webUI 未启用时直接 404。
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.webUI == nil {
			http.NotFound(w, r)
			return
		}
		if s.validSession(r) {
			next(w, r)
			return
		}
		var tok string
		if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
			tok = strings.TrimPrefix(ah, "Bearer ")
		} else {
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.webUI.adminToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
