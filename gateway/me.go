package gateway

import (
	"encoding/json"
	"net/http"

	"nextemby-replay/engine"
)

// meUserID 取个人中心的用户身份：X-NB-User 头优先，其次 ?nb_user= 参数。
// MVP 说明：身份识别后续接登录体系，见 README。
func meUserID(r *http.Request) string {
	if u := r.Header.Get("X-NB-User"); u != "" {
		return u
	}
	return r.URL.Query().Get("nb_user")
}

func (s *Server) meUser(w http.ResponseWriter, r *http.Request) (engine.User, string, bool) {
	if s.webUI == nil {
		http.NotFound(w, r)
		return engine.User{}, "", false
	}
	id := meUserID(r)
	if id == "" {
		http.Error(w, "missing nb_user", http.StatusBadRequest)
		return engine.User{}, "", false
	}
	u, err := s.engine.Users().GetUser(r.Context(), id)
	if err != nil {
		http.Error(w, "unknown user", http.StatusNotFound)
		return engine.User{}, "", false
	}
	return u, id, true
}

func (s *Server) handleMePage(w http.ResponseWriter, r *http.Request) {
	if s.webUI == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(mustReadWebFile("web/me.html"))
}

func (s *Server) handleMeProfile(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.meUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	okN, failN := s.engine.Stats(id)
	var last []decisionJSON
	if s.db != nil {
		// DB 优先：用户统计与近期播放从 decisions 表聚合。
		if us, err := s.db.UserStats(ctx); err == nil {
			if st, ok := us[id]; ok {
				okN, failN = st.OK, st.Fail
			} else {
				okN, failN = 0, 0
			}
		}
		if recs, err := s.db.RecentUserDecisions(ctx, id, 5); err == nil {
			for _, d := range recs {
				last = append(last, toDecisionJSON(d))
			}
		}
	} else {
		recent := s.engine.RecentDecisions(200)
		for _, d := range recent {
			if d.UserID == id {
				last = append(last, toDecisionJSON(d))
				if len(last) >= 5 {
					break
				}
			}
		}
	}
	writeJSON(w, map[string]any{
		"userID":    u.ID,
		"mode":      u.Mode,
		"template":  u.Template,
		"hasCookie": s.webUI.cookies.HasCookie(ctx, id),
		"stats":     map[string]int64{"ok": okN, "fail": failN},
		"recent":    last,
	})
}

// handleMeCookie 保存用户 115 Cookie：
// 先做 LoginCheck 校验（失效 400 不存），再 AES-GCM 加密存储，
// 同时把 client 接入引擎（115 模式 STEP1 的"用户自有盘"即走它）。
func (s *Server) handleMeCookie(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.meUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Cookie == "" {
		http.Error(w, "cookie must not be empty", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	var client engine.DriveClient
	if s.webUI.validateCookie != nil {
		c, err := s.webUI.validateCookie(ctx, id, body.Cookie)
		if err != nil {
			s.logger.Printf("me cookie 校验失败 user=%s: %v", id, err)
			http.Error(w, "invalid_cookie", http.StatusBadRequest)
			return
		}
		client = c
	}
	if err := s.webUI.cookies.SetCookie(ctx, id, body.Cookie); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Cookie 绝不打日志：只记用户 ID。
	s.logger.Printf("me cookie 已保存 user=%s", id)
	if client != nil {
		s.webUI.mu.Lock()
		s.webUI.userClients[id] = client
		s.webUI.mu.Unlock()
		s.engine.SetOwnDrive(id, client)
	}
	writeJSON(w, map[string]any{"ok": true, "userID": u.ID})
}

// handleMeMode 切换播放模式：115（用自己网盘）/ pool（走分布式池）。
func (s *Server) handleMeMode(w http.ResponseWriter, r *http.Request) {
	u, id, ok := s.meUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Mode != engine.ModeOwn115 && body.Mode != engine.ModePool {
		http.Error(w, "mode must be 115 or pool", http.StatusBadRequest)
		return
	}
	updater, ok := s.engine.Users().(engine.UserUpdater)
	if !ok {
		http.Error(w, "mode switch not implemented by user store (待接 DB)", http.StatusNotImplemented)
		return
	}
	u.Mode = body.Mode
	if err := updater.UpdateUser(r.Context(), u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{"ok": true, "mode": u.Mode}
	if body.Mode == engine.ModeOwn115 && !s.webUI.cookies.HasCookie(r.Context(), id) {
		resp["warn"] = "no_cookie_bound"
	}
	writeJSON(w, resp)
}

// lookupUserClient 返回用户自有 115 client（懒加载缓存；网关内部用）。
func (s *Server) lookupUserClient(userID string) (engine.DriveClient, bool) {
	if s.webUI == nil {
		return nil, false
	}
	s.webUI.mu.Lock()
	defer s.webUI.mu.Unlock()
	c, ok := s.webUI.userClients[userID]
	return c, ok
}
