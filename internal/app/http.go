package app

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"golang.org/x/oauth2"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed web/*
var assets embed.FS

func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, e error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": e.Error()})
}
func equal(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (a *App) Handler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	web, _ := fs.Sub(assets, "web")
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "mailroom", Value: a.Session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.FileServer(http.FS(web)).ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		a.statusMu.Lock()
		busy, last := a.busy, a.lastError
		a.statusMu.Unlock()
		all, e := a.Store.List("")
		if e != nil {
			apiError(w, e)
			return
		}
		counts := map[string]int{}
		for _, m := range all {
			counts[m.State]++
		}
		connected := a.Gmail != nil && a.Gmail.Connected()
		googleReady := a.Gmail != nil && a.Gmail.Config != nil
		jsonReply(w, map[string]any{"demo": a.C.Demo, "connected": connected, "google_ready": googleReady, "openai_ready": a.C.OpenAIKey != "" && a.C.Model != "", "model": a.C.Model, "account": a.Store.Meta("account"), "last_sync": a.Store.Meta("last_sync"), "busy": busy, "error": last, "counts": counts, "csrf": a.CSRF, "codex_auto": a.C.AutoCodex, "codex_actions": a.C.AllowCodexActions, "poll_seconds": a.C.PollSeconds, "data_dir": a.C.DataDir})
	})
	mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		rows, e := a.Store.List(r.URL.Query().Get("state"))
		if e != nil {
			apiError(w, e)
			return
		}
		if equal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), a.AgentToken) && len(rows) > 20 {
			rows = rows[:20]
		}
		for i := range rows {
			rows[i].Body = ""
		}
		jsonReply(w, rows)
	})
	mux.HandleFunc("GET /api/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		m, e := a.Store.Get(r.PathValue("id"))
		if e != nil {
			http.Error(w, "未找到邮件", 404)
			return
		}
		jsonReply(w, m)
	})
	mux.HandleFunc("GET /api/messages/{id}/context", func(w http.ResponseWriter, r *http.Request) {
		m, e := a.Store.Get(r.PathValue("id"))
		if e != nil {
			apiError(w, e)
			return
		}
		thread := []Message{}
		if !a.C.Demo && a.Gmail != nil {
			thread, e = a.Gmail.ThreadContext(r.Context(), m.ThreadID)
			if e != nil {
				apiError(w, e)
				return
			}
		}
		jsonReply(w, map[string]any{"email": m, "conversation": thread})
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		v, e := a.Store.Events()
		if e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, v)
	})
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		go a.Tick(ctx)
		jsonReply(w, map[string]bool{"scheduled": true})
	})
	mux.HandleFunc("POST /api/import", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			if e := a.Import(ctx); e != nil {
				a.setStatus(false, e)
			} else {
				a.Tick(ctx)
			}
		}()
		jsonReply(w, map[string]bool{"scheduled": true})
	})
	mux.HandleFunc("POST /api/messages/{id}/action", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Action string `json:"action"`
		}
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); e != nil {
			apiError(w, e)
			return
		}
		agent := equal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), a.AgentToken)
		if agent && in.Action == "retry" {
			http.Error(w, "agent cannot reset classification", 403)
			return
		}
		if e := a.Action(r.Context(), r.PathValue("id"), in.Action, agent); e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/messages/{id}/review", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		}
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); e != nil {
			apiError(w, e)
			return
		}
		if e := a.review(r.Context(), r.PathValue("id"), in.Verdict, in.Reason, equal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), a.AgentToken)); e != nil {
			apiError(w, e)
			return
		}
		jsonReply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/codex", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			if e := a.Dispatch(ctx); e != nil {
				_ = a.Store.Log("", "codex_error", e.Error())
			}
		}()
		jsonReply(w, map[string]bool{"scheduled": true})
	})
	mux.HandleFunc("POST /api/connect", func(w http.ResponseWriter, r *http.Request) {
		if a.C.Demo || a.Gmail == nil || a.Gmail.Config == nil {
			apiError(w, fmt.Errorf("请先配置 credentials.json 并重新启动应用"))
			return
		}
		state, verifier := secret(), oauth2.GenerateVerifier()
		a.oauthMu.Lock()
		for k, v := range a.oauthStates {
			if time.Now().After(v.Expires) {
				delete(a.oauthStates, k)
			}
		}
		a.oauthStates[state] = oauthState{verifier, time.Now().Add(10 * time.Minute)}
		a.oauthMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "oauth_state", Value: state, Path: "/oauth/callback", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
		u := a.Gmail.Config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier))
		jsonReply(w, map[string]string{"url": u})
	})
	mux.HandleFunc("GET /oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		cookie, e := r.Cookie("oauth_state")
		a.oauthMu.Lock()
		s, ok := a.oauthStates[state]
		delete(a.oauthStates, state)
		a.oauthMu.Unlock()
		if e != nil || !equal(cookie.Value, state) || !ok || time.Now().After(s.Expires) {
			http.Error(w, "授权状态无效或已过期，请从页面重新连接。", 400)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "oauth_state", Path: "/oauth/callback", MaxAge: -1})
		if r.URL.Query().Get("error") != "" {
			http.Error(w, "Google 授权未完成。", 400)
			return
		}
		t, e := a.Gmail.Config.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(s.Verifier))
		if e != nil {
			http.Error(w, "授权交换失败，请重试。", 400)
			return
		}
		temp := &Gmail{Config: a.Gmail.Config, TokenPath: a.Gmail.TokenPath, Base: a.Gmail.Base}
		temp.setToken(t)
		p, e := temp.Profile(r.Context())
		if e == nil {
			e = a.Store.BindAccount(p.Email)
		}
		if e == nil {
			e = a.Gmail.AcceptToken(t)
		}
		if e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		a.setStatus(false, nil)
		go a.Tick(ctx)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.Host != a.C.Addr {
			http.Error(w, "Invalid Host", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Scheme != "http" || u.Host != a.C.Addr {
				http.Error(w, "Origin not allowed", 403)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			agent := equal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), a.AgentToken)
			cookie, e := r.Cookie("mailroom")
			browser := e == nil && equal(cookie.Value, a.Session)
			if !agent && !browser {
				http.Error(w, "Unauthorized", 401)
				return
			}
			if r.Method != "GET" && !agent && !equal(r.Header.Get("X-CSRF-Token"), a.CSRF) {
				http.Error(w, "Invalid CSRF token", 403)
				return
			}
			if agent && (r.URL.Path == "/api/connect" || r.URL.Path == "/api/import" || r.URL.Path == "/api/codex" || r.URL.Path == "/api/sync" || r.URL.Path == "/api/status") {
				http.Error(w, "Agent endpoint denied", 403)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
