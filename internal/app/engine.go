package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type App struct {
	C           Config
	Store       *Store
	Gmail       *Gmail
	Session     string
	CSRF        string
	AgentToken  string
	work        sync.Mutex
	oauthMu     sync.Mutex
	oauthStates map[string]oauthState
	statusMu    sync.Mutex
	lastError   string
	busy        bool
	dispatchMu  sync.Mutex
}
type oauthState struct {
	Verifier string
	Expires  time.Time
}

func secret() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func New(c Config) (*App, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	s, e := OpenStore(filepath.Join(c.DataDir, "mailroom.db"))
	if e != nil {
		return nil, e
	}
	mode := "live"
	if c.Demo {
		mode = "demo"
	}
	if old := s.Meta("mode"); old != "" && old != mode {
		s.Close()
		return nil, errors.New("演示和真实邮箱必须使用不同的数据目录")
	}
	if e := s.SetMeta("mode", mode); e != nil {
		s.Close()
		return nil, e
	}
	a := &App{C: c, Store: s, Session: secret(), CSRF: secret(), AgentToken: secret(), oauthStates: map[string]oauthState{}}
	g, e := NewGmail(c)
	a.Gmail = g
	if e != nil {
		a.lastError = "尚未配置 Google OAuth，页面可正常使用。"
	}
	if c.Demo {
		a.Gmail = nil
		a.lastError = ""
		if s.Meta("demo_seeded") == "" {
			if e = a.seed(); e != nil {
				s.Close()
				return nil, e
			}
		}
	}
	return a, nil
}
func (a *App) setStatus(b bool, e error) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.busy = b
	if e != nil {
		a.lastError = e.Error()
	} else {
		a.lastError = ""
	}
}
func (a *App) Run(ctx context.Context) {
	a.Tick(ctx)
	t := time.NewTicker(time.Duration(a.C.PollSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Tick(ctx)
		}
	}
}
func (a *App) Tick(ctx context.Context) {
	if !a.work.TryLock() {
		return
	}
	defer a.work.Unlock()
	if a.C.Demo {
		return
	}
	if a.Gmail == nil || !a.Gmail.Connected() {
		return
	}
	a.setStatus(true, nil)
	e := a.sync(ctx)
	if e == nil {
		e = a.process(ctx)
	}
	a.setStatus(false, e)
	if e != nil {
		_ = a.Store.Log("", "sync_error", e.Error())
	}
	if e == nil && a.C.AutoCodex {
		go func() {
			if e := a.Dispatch(ctx); e != nil {
				log.Printf("Codex: %v", e)
			}
		}()
	}
}
func (a *App) sync(ctx context.Context) error {
	p, e := a.Gmail.Profile(ctx)
	if e != nil {
		return e
	}
	if e = a.Store.BindAccount(p.Email); e != nil {
		return e
	}
	cursor := a.Store.Meta("history_id")
	if cursor == "" {
		if e = a.Store.SetMeta("monitor_since", strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)); e != nil {
			return e
		}
		if e = a.Store.SetMeta("history_id", p.History); e != nil {
			return e
		}
		_ = a.Store.Log("", "monitor_started", "从现在开始监控；历史邮件需手动导入最近 7 天。")
		return nil
	}
	ids, next, e := a.Gmail.History(ctx, cursor)
	var he *HTTPError
	if errors.As(e, &he) && he.Code == 404 {
		since := a.Store.Meta("monitor_since")
		if since == "" {
			return errors.New("缺少同步起点，无法安全恢复")
		}
		ids, e = a.Gmail.ListIDs(ctx, "in:inbox after:"+since)
		next = p.History
		_ = a.Store.Log("", "history_recovery", "历史游标失效，按监控起点补查收件箱。")
	}
	if e != nil {
		return e
	}
	if e = a.enqueue(ctx, ids); e != nil {
		return e
	}
	if e = a.Store.SetMeta("history_id", next); e != nil {
		return e
	}
	return a.Store.SetMeta("last_sync", time.Now().UTC().Format(time.RFC3339))
}
func (a *App) enqueue(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, e := a.Store.Get(id); e == nil {
			continue
		}
		m, e := a.Gmail.Fetch(ctx, id)
		var he *HTTPError
		if errors.As(e, &he) && he.Code == 404 {
			continue
		}
		if e != nil {
			return e
		}
		if !contains(m.Labels, "INBOX") || contains(m.Labels, "TRASH") {
			continue
		}
		if e = a.Store.Put(m); e != nil {
			return e
		}
	}
	return nil
}
func (a *App) Import(ctx context.Context) error {
	if !a.work.TryLock() {
		return errors.New("后台正在处理邮件，请稍后重试")
	}
	defer a.work.Unlock()
	if a.C.Demo {
		return nil
	}
	if a.Gmail == nil || !a.Gmail.Connected() {
		return errors.New("请先连接 Gmail")
	}
	p, e := a.Gmail.Profile(ctx)
	if e != nil {
		return e
	}
	if e = a.Store.BindAccount(p.Email); e != nil {
		return e
	}
	ids, e := a.Gmail.ListIDs(ctx, "in:inbox newer_than:7d")
	if e != nil {
		return e
	}
	if e = a.enqueue(ctx, ids); e != nil {
		return e
	}
	return a.Store.Log("", "import", fmt.Sprintf("导入最近 7 天的 %d 封邮件，重复 ID 自动跳过", len(ids)))
}
func (a *App) process(ctx context.Context) error {
	if a.C.OpenAIKey == "" || a.C.Model == "" {
		return errors.New("已同步；请设置 OPENAI_API_KEY 和 OPENAI_MODEL 后分类")
	}
	rows, e := a.Store.List("")
	if e != nil {
		return e
	}
	var first error
	count := 0
	for i := len(rows) - 1; i >= 0; i-- {
		m := rows[i]
		if m.State != "queued" && m.State != "error" && m.State != "label_pending" {
			continue
		}
		if m.NextTry > time.Now().Unix() {
			continue
		}
		if count >= 5 {
			break
		}
		count++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.State != "label_pending" {
			fresh, err := a.Gmail.Fetch(ctx, m.ID)
			if err != nil {
				a.fail(&m, err)
				if first == nil {
					first = err
				}
				continue
			}
			if !contains(fresh.Labels, "INBOX") {
				m.State = "outside"
				_ = a.Store.Put(m)
				continue
			}
			m.Body = fresh.Body
			m.Labels = fresh.Labels
			m.Attachments = fresh.Attachments
			m.Truncated = fresh.Truncated
			thread, err := a.Gmail.ThreadContext(ctx, m.ThreadID)
			if err == nil {
				var d Decision
				d, err = classify(ctx, a.C, m, thread)
				if err == nil {
					m.Decision = &d
					m.Model = a.C.Model + " / " + policyVersion
					m.State = "label_pending"
					m.Error = ""
					m.NextTry = 0
					if err = a.Store.Put(m); err != nil {
						return err
					}
				}
			}
			if err != nil {
				a.fail(&m, err)
				if first == nil {
					first = err
				}
				continue
			}
		}
		if err := a.label(ctx, &m); err != nil {
			a.fail(&m, err)
			if first == nil {
				first = err
			}
			continue
		}
		_ = a.Store.Log(m.ID, "classified", m.Decision.Reason)
	}
	return first
}
func (a *App) fail(m *Message, e error) {
	if m.Decision != nil {
		m.State = "label_pending"
	} else {
		m.State = "error"
	}
	m.Error = e.Error()
	m.Attempts++
	shift := min(m.Attempts, 8)
	m.NextTry = time.Now().Add(time.Duration(1<<shift) * time.Minute).Unix()
	_ = a.Store.Put(*m)
	_ = a.Store.Log(m.ID, "retry_scheduled", e.Error())
}
func (a *App) label(ctx context.Context, m *Message) error {
	if m.Decision == nil {
		return errors.New("缺少分类结果")
	}
	state := decisionState(*m.Decision)
	if a.C.Demo {
		m.State = state
		m.Error = ""
		return a.Store.Put(*m)
	}
	labels, e := a.Gmail.EnsureLabels(ctx)
	if e != nil {
		return e
	}
	name := map[string]string{"keep": "AI/保留", "review": "AI/待复核", "promotion": "AI/推广"}[state]
	add := []string{labels[name]}
	remove := []string{}
	for _, n := range []string{"AI/保留", "AI/待复核", "AI/推广"} {
		if n != name {
			remove = append(remove, labels[n])
		}
	}
	if e = a.Gmail.Modify(ctx, m.ID, add, remove); e != nil {
		return e
	}
	fresh, e := a.Gmail.Fetch(ctx, m.ID)
	if e != nil {
		return e
	}
	if !contains(fresh.Labels, labels[name]) {
		return errors.New("Gmail 标签核验失败")
	}
	m.Labels = fresh.Labels
	m.State = state
	m.Error = ""
	m.Attempts = 0
	m.NextTry = 0
	return a.Store.Put(*m)
}
func (a *App) Action(ctx context.Context, id, action string, agent bool) error {
	if !a.work.TryLock() {
		return errors.New("后台正在处理邮件，请稍后重试")
	}
	defer a.work.Unlock()
	m, e := a.Store.Get(id)
	if e != nil {
		return e
	}
	if action == "retry" {
		m.State = "queued"
		m.Decision = nil
		m.NextTry = 0
		m.Error = ""
		return a.Store.Put(m)
	}
	if action != "archive" && action != "trash" && action != "restore" {
		return errors.New("不支持的操作")
	}
	if agent && (!a.C.AllowCodexActions || m.Decision == nil || m.Decision.Protected || m.State != "promotion") {
		return errors.New("Codex 仅可复核；清理需要页面操作或显式启用允许的动作")
	}
	if action != "restore" && (m.State == "archived" || m.State == "trashed") {
		return nil
	}
	if action == "restore" && m.State != "archived" && m.State != "trashed" && m.PendingAction == "" {
		return errors.New("只能恢复本应用已归档或移入垃圾箱的邮件")
	}
	add, remove := []string{}, []string{}
	state := ""
	switch action {
	case "archive":
		remove = []string{"INBOX"}
		state = "archived"
	case "trash":
		add = []string{"TRASH"}
		remove = []string{"INBOX"}
		state = "trashed"
	case "restore":
		add = []string{"INBOX"}
		remove = []string{"TRASH"}
		state = "review"
	}
	if !a.C.Demo {
		if a.Gmail == nil {
			return errors.New("Gmail 尚未连接")
		}
		fresh, err := a.Gmail.Fetch(ctx, id)
		if err != nil {
			return err
		}
		if action != "restore" && !contains(fresh.Labels, "INBOX") && m.PendingAction != action {
			return errors.New("邮件已不在收件箱，刷新后再操作")
		}
		m.PendingAction = action
		if e = a.Store.Put(m); e != nil {
			return e
		}
		if e = a.Gmail.Modify(ctx, id, add, remove); e != nil {
			m.Error = "操作未确认，可重试同一动作：" + e.Error()
			_ = a.Store.Put(m)
			return e
		}
		fresh, e = a.Gmail.Fetch(ctx, id)
		if e != nil {
			m.Error = "操作已发送但核验失败，可重试同一动作：" + e.Error()
			_ = a.Store.Put(m)
			return e
		}
		for _, x := range add {
			if !contains(fresh.Labels, x) {
				return errors.New("操作核验失败")
			}
		}
		for _, x := range remove {
			if contains(fresh.Labels, x) {
				return errors.New("操作核验失败")
			}
		}
		m.Labels = fresh.Labels
	}
	m.State = state
	m.PendingAction = ""
	m.Error = ""
	if e = a.Store.Put(m); e != nil {
		return e
	}
	return a.Store.Log(id, action, "动作完成并核验")
}
func (a *App) Review(ctx context.Context, id, verdict, reason string) error {
	return a.review(ctx, id, verdict, reason, false)
}
func (a *App) review(ctx context.Context, id, verdict, reason string, agent bool) error {
	if !a.work.TryLock() {
		return errors.New("后台正在处理邮件，请稍后重试")
	}
	defer a.work.Unlock()
	if verdict != "keep" && verdict != "promotion" {
		return errors.New("复核结果只能是 keep 或 promotion")
	}
	if reason == "" {
		return errors.New("请提供复核依据")
	}
	m, e := a.Store.Get(id)
	if e != nil {
		return e
	}
	if m.State == "archived" || m.State == "trashed" || m.State == "outside" {
		return errors.New("邮件已经离开复核队列")
	}
	d := Decision{Category: "other", Confidence: 1, Reason: reason, Evidence: []string{}, Protected: verdict == "keep"}
	if verdict == "promotion" {
		if agent && (m.Attachments || m.Truncated) {
			return errors.New("缺少完整正文或附件信息，需用户复核")
		}
		if m.Decision != nil && m.Decision.Protected {
			return errors.New("受保护邮件不能直接降级为推广")
		}
		d.Category = "promotion"
	}
	m.Decision = &d
	if e = a.label(ctx, &m); e != nil {
		return e
	}
	return a.Store.Log(id, "reviewed", reason)
}
func (a *App) seed() error {
	samples := []struct {
		id, from, subject, body, cat, state, reason string
		score                                       float64
		protected                                   bool
	}{
		{"demo-1", "Luna Studio <news@example.com>", "春季更新：让你的工作流更简单", "了解新的协作功能，查看发布说明。此邮件是合成演示数据。", "product_update", "promotion", "普通产品功能介绍，没有账户或付款变更。", .98, false},
		{"demo-2", "Example Bank <billing@example.com>", "你的月度账单已生成", "本期账户账单已生成。请在账户页面查看明细。", "bill", "keep", "账单属于必须保留的记录。", .99, true},
		{"demo-3", "Alex <alex@example.com>", "关于下周的项目沟通", "下周三可以一起讨论项目吗？附件是方案。", "work", "keep", "实际项目沟通，保留私人和工作往来。", .99, true},
		{"demo-4", "Cloud Notes <updates@example.com>", "新功能与免费计划调整", "新编辑器上线，同时免费计划的存储额度下月将调整。", "other", "review", "混合产品资讯与计划变化，需要进一步复核。", .72, false},
		{"demo-5", "Reading Weekly <digest@example.com>", "本周值得读的五篇文章", "本周阅读精选与公开课程推荐。", "newsletter", "promotion", "自动发送的普通阅读周报。", .97, false},
		{"demo-6", "Travel Club <hello@example.com>", "你的下一次旅行，从这里开始", "夏季旅行灵感和优惠活动。", "promotion", "promotion", "旅行活动推广，未包含实际订单。", .98, false},
	}
	for i, x := range samples {
		d := &Decision{Category: x.cat, Confidence: x.score, Reason: x.reason, Evidence: []string{x.body}, Protected: x.protected, NeedsReview: x.state == "review"}
		m := Message{ID: x.id, ThreadID: x.id, From: x.from, Subject: x.subject, Body: x.body, Date: time.Now().Add(-time.Duration(i) * time.Hour).UTC().Format(time.RFC3339), Labels: []string{"INBOX"}, Decision: d, State: x.state, Model: "合成示例 / 无 API 调用"}
		if e := a.Store.Put(m); e != nil {
			return e
		}
	}
	return a.Store.SetMeta("demo_seeded", "1")
}
