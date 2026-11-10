package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/html"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const gmailScope = "https://www.googleapis.com/auth/gmail.modify"

type Gmail struct {
	Config    *oauth2.Config
	TokenPath string
	Base      string
	mu        sync.Mutex
	client    *http.Client
}
type savedSource struct {
	mu     sync.Mutex
	source oauth2.TokenSource
	path   string
	last   string
}

func (s *savedSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, e := s.source.Token()
	if e != nil {
		return nil, e
	}
	if t.AccessToken != s.last {
		if e = atomicJSON(s.path, t); e != nil {
			return nil, e
		}
		s.last = t.AccessToken
	}
	return t, nil
}
func NewGmail(c Config) (*Gmail, error) {
	g := &Gmail{TokenPath: filepath.Join(c.DataDir, "google-token.json"), Base: "https://gmail.googleapis.com/gmail/v1/users/me"}
	b, e := os.ReadFile(c.GoogleCredentials)
	if e != nil {
		return g, e
	}
	g.Config, e = google.ConfigFromJSON(b, gmailScope)
	if e != nil {
		return g, e
	}
	g.Config.RedirectURL = "http://" + c.Addr + "/oauth/callback"
	if b, e = os.ReadFile(g.TokenPath); e == nil {
		var t oauth2.Token
		if e = json.Unmarshal(b, &t); e != nil {
			return g, e
		}
		g.setToken(&t)
	}
	return g, nil
}
func (g *Gmail) setToken(t *oauth2.Token) {
	g.mu.Lock()
	defer g.mu.Unlock()
	oauthCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second})
	s := &savedSource{source: g.Config.TokenSource(oauthCtx, t), path: g.TokenPath}
	g.client = oauth2.NewClient(context.Background(), oauth2.ReuseTokenSource(t, s))
	g.client.Timeout = 45 * time.Second
}
func (g *Gmail) Connected() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.client != nil }
func (g *Gmail) AcceptToken(t *oauth2.Token) error {
	if t.RefreshToken == "" {
		return errors.New("Google 未返回 refresh token，请撤销本应用旧授权后重新连接")
	}
	if e := atomicJSON(g.TokenPath, t); e != nil {
		return e
	}
	g.setToken(t)
	return nil
}

type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("Gmail API HTTP %d", e.Code) }
func (g *Gmail) Do(ctx context.Context, method, path string, input, output any) error {
	g.mu.Lock()
	client := g.client
	g.mu.Unlock()
	if client == nil {
		return errors.New("Gmail 尚未连接")
	}
	var payload []byte
	var err error
	if input != nil {
		payload, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	for n := 0; n < 3; n++ {
		req, e := http.NewRequestWithContext(ctx, method, g.Base+path, bytes.NewReader(payload))
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/json")
		res, e := client.Do(req)
		if e != nil {
			return errors.New("Gmail 请求失败，请检查网络或重新连接账户")
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, 12<<20))
		res.Body.Close()
		if e != nil {
			return e
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if output != nil && len(b) > 0 {
				return json.Unmarshal(b, output)
			}
			return nil
		}
		if (res.StatusCode == 429 || res.StatusCode >= 500) && n < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<n) * time.Second):
			}
			continue
		}
		return &HTTPError{res.StatusCode}
	}
	return errors.New("Gmail 重试失败")
}

type Profile struct {
	Email   string `json:"emailAddress"`
	History string `json:"historyId"`
}

func (g *Gmail) Profile(ctx context.Context) (Profile, error) {
	var p Profile
	e := g.Do(ctx, "GET", "/profile", nil, &p)
	return p, e
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type Part struct {
	Mime     string   `json:"mimeType"`
	Filename string   `json:"filename"`
	Headers  []Header `json:"headers"`
	Body     struct {
		Data         string `json:"data"`
		Size         int    `json:"size"`
		AttachmentID string `json:"attachmentId"`
	} `json:"body"`
	Parts []Part `json:"parts"`
}
type GmailMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	Labels       []string `json:"labelIds"`
	InternalDate string   `json:"internalDate"`
	Snippet      string   `json:"snippet"`
	Payload      Part     `json:"payload"`
}

func (g *Gmail) Fetch(ctx context.Context, id string) (Message, error) {
	var v GmailMessage
	e := g.Do(ctx, "GET", "/messages/"+url.PathEscape(id)+"?format=full", nil, &v)
	if e != nil {
		return Message{}, e
	}
	return parseMessage(v), nil
}
func parseMessage(v GmailMessage) Message {
	m := Message{ID: v.ID, ThreadID: v.ThreadID, Labels: v.Labels, State: "queued"}
	for _, h := range v.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			m.From = h.Value
		case "subject":
			m.Subject = h.Value
		}
	}
	ms, _ := strconv.ParseInt(v.InternalDate, 10, 64)
	m.Date = time.UnixMilli(ms).UTC().Format(time.RFC3339)
	var plain, rich []string
	var walk func(Part)
	walk = func(p Part) {
		if p.Filename != "" {
			m.Attachments = true
			return
		}
		b, _ := base64.RawURLEncoding.DecodeString(p.Body.Data)
		if len(b) == 0 && p.Body.Size > 0 {
			m.Truncated = true
		}
		if p.Mime == "text/plain" {
			plain = append(plain, string(b))
		}
		if p.Mime == "text/html" {
			rich = append(rich, htmlText(string(b)))
		}
		for _, c := range p.Parts {
			walk(c)
		}
	}
	walk(v.Payload)
	m.Body = strings.Join(plain, "\n")
	if strings.TrimSpace(m.Body) == "" {
		m.Body = strings.Join(rich, "\n")
	}
	if strings.TrimSpace(m.Body) == "" {
		m.Body = v.Snippet
		m.Truncated = true
	}
	runes := []rune(m.Body)
	if len(runes) > 60000 {
		m.Body = string(runes[:60000])
		m.Truncated = true
	}
	return m
}
func htmlText(s string) string {
	doc, e := html.Parse(strings.NewReader(s))
	if e != nil {
		return ""
	}
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "head") {
			return
		}
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
			out.WriteByte(' ')
		}
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, a := range n.Attr {
				if a.Key == "alt" {
					out.WriteString(a.Val)
					out.WriteByte(' ')
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.Join(strings.Fields(out.String()), " ")
}
func (g *Gmail) ListIDs(ctx context.Context, q string) ([]string, error) {
	ids := []string{}
	p := ""
	for {
		v := url.Values{"q": {q}, "maxResults": {"100"}}
		if p != "" {
			v.Set("pageToken", p)
		}
		var r struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			Next string `json:"nextPageToken"`
		}
		if e := g.Do(ctx, "GET", "/messages?"+v.Encode(), nil, &r); e != nil {
			return nil, e
		}
		for _, m := range r.Messages {
			ids = append(ids, m.ID)
		}
		p = r.Next
		if p == "" {
			return ids, nil
		}
	}
}
func (g *Gmail) History(ctx context.Context, start string) ([]string, string, error) {
	seen := map[string]bool{}
	p := ""
	latest := start
	for {
		v := url.Values{"startHistoryId": {start}, "maxResults": {"100"}}
		if p != "" {
			v.Set("pageToken", p)
		}
		var r struct {
			HistoryID string `json:"historyId"`
			Next      string `json:"nextPageToken"`
			History   []struct {
				Added []struct {
					Message struct {
						ID string `json:"id"`
					} `json:"message"`
				} `json:"messagesAdded"`
				LabelsAdded []struct {
					Message struct {
						ID string `json:"id"`
					} `json:"message"`
					Labels []string `json:"labelIds"`
				} `json:"labelsAdded"`
			} `json:"history"`
		}
		if e := g.Do(ctx, "GET", "/history?"+v.Encode(), nil, &r); e != nil {
			return nil, "", e
		}
		for _, h := range r.History {
			for _, x := range h.Added {
				seen[x.Message.ID] = true
			}
			for _, x := range h.LabelsAdded {
				if contains(x.Labels, "INBOX") {
					seen[x.Message.ID] = true
				}
			}
		}
		if r.HistoryID != "" {
			latest = r.HistoryID
		}
		p = r.Next
		if p == "" {
			break
		}
	}
	ids := []string{}
	for id := range seen {
		ids = append(ids, id)
	}
	return ids, latest, nil
}
func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func (g *Gmail) ThreadContext(ctx context.Context, id string) ([]Message, error) {
	var r struct {
		Messages []GmailMessage `json:"messages"`
	}
	if e := g.Do(ctx, "GET", "/threads/"+url.PathEscape(id)+"?format=full", nil, &r); e != nil {
		return nil, e
	}
	out := []Message{}
	for _, m := range r.Messages {
		out = append(out, parseMessage(m))
	}
	return out, nil
}
func (g *Gmail) EnsureLabels(ctx context.Context) (map[string]string, error) {
	var r struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	if e := g.Do(ctx, "GET", "/labels", nil, &r); e != nil {
		return nil, e
	}
	out := map[string]string{}
	for _, l := range r.Labels {
		out[l.Name] = l.ID
	}
	for _, n := range []string{"AI/推广", "AI/保留", "AI/待复核"} {
		if out[n] == "" {
			var l struct {
				ID string `json:"id"`
			}
			e := g.Do(ctx, "POST", "/labels", map[string]any{"name": n, "labelListVisibility": "labelShow", "messageListVisibility": "show"}, &l)
			if e != nil {
				return nil, e
			}
			out[n] = l.ID
		}
	}
	return out, nil
}
func (g *Gmail) Modify(ctx context.Context, id string, add, remove []string) error {
	return g.Do(ctx, "POST", "/messages/"+url.PathEscape(id)+"/modify", map[string]any{"addLabelIds": add, "removeLabelIds": remove}, nil)
}
