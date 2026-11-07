package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// SEARCH_PROVIDER=searxng: web_search goes to your own SearXNG instance and web_fetch is done by this program
// (stdlib only). Because the model decides which URLs get fetched, the fetcher refuses loopback, private,
// link-local and CGNAT addresses unless FETCH_ALLOW_PRIVATE=true.

const (
	fetchMaxBytes     = 2 << 20
	fetchTimeout      = 20 * time.Second
	fetchMaxRedirects = 5
)

var (
	errBlockedHost = errors.New("出于安全考虑，不允许访问内网、本机或链路本地地址（FETCH_ALLOW_PRIVATE=true 可放开）")
	cgnat          = netip.MustParsePrefix("100.64.0.0/10")
)

func blockedAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || cgnat.Contains(a)
}

func proxyConfigured() bool {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// checkFetchHost rejects hosts that are, or resolve to, a blocked address.
func (a *App) checkFetchHost(ctx context.Context, host string) error {
	if a.cfg.FetchAllowPrivate {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if blockedAddr(ip) {
			return errBlockedHost
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return errors.New("无法解析主机名")
	}
	for _, ip := range addrs {
		if blockedAddr(ip) {
			return errBlockedHost
		}
	}
	return nil
}

// guardedDial resolves the host itself, refuses blocked addresses and dials the validated IP, so a DNS answer
// that changes between the check and the connection cannot reach an internal address.
func guardedDial(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("无法解析主机名")
		}
		for _, ip := range ips {
			if blockedAddr(ip) {
				return nil, errBlockedHost
			}
		}
		var last error
		for _, ip := range ips {
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

// newFetchClient builds the HTTP client used by web_fetch. Without a configured proxy the dial itself is
// guarded; with one (the dial goes to the proxy) every request and redirect hop is checked by host instead.
func newFetchClient(allowPrivate bool, check func(ctx context.Context, host string) error) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	if allowPrivate || proxyConfigured() {
		tr.Proxy = http.ProxyFromEnvironment
		tr.DialContext = d.DialContext
	} else {
		tr.DialContext = guardedDial(d)
	}
	return &http.Client{
		Transport: tr,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= fetchMaxRedirects {
				return errors.New("重定向次数过多")
			}
			if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.User != nil {
				return errors.New("重定向到了不支持的地址")
			}
			return check(req.Context(), req.URL.Hostname())
		},
	}
}

func cleanNetErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, errBlockedHost) {
		return errBlockedHost
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("请求超时")
	}
	return errors.New("无法访问该页面")
}

// fetchPage downloads one page (http/https) and returns its title and plain text.
func (a *App) fetchPage(ctx context.Context, u *url.URL) (title, text string, err error) {
	if err := a.checkFetchHost(ctx, u.Hostname()); err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; memory-bot)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1")
	resp, err := a.fetchc.Do(req)
	if err != nil {
		return "", "", cleanNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	switch {
	case mt == "", strings.HasPrefix(mt, "text/"), mt == "application/xhtml+xml", mt == "application/json", mt == "application/xml":
	default:
		return "", "", fmt.Errorf("不支持的内容类型：%s", mt)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil && len(body) == 0 {
		return "", "", cleanNetErr(err)
	}
	s := strings.ToValidUTF8(string(body), "")
	head := strings.ToLower(s[:min(len(s), 512)])
	if mt == "text/html" || mt == "application/xhtml+xml" || (mt == "" && (strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html"))) {
		title, text = htmlToText(s)
		return title, text, nil
	}
	return "", strings.TrimSpace(s), nil
}

var (
	reComments = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlock    = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|tr|table|h[1-6]|section|article|header|footer|nav|aside|blockquote|pre|hr|form|dd|dt|dl|figure|figcaption|main)\b[^>]*>`)
	reTags     = regexp.MustCompile(`(?s)<[^>]*>`)
	reTitle    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reSpaces   = regexp.MustCompile(`[ \t\f\v\r\x{00a0}]+`)
	reBlank    = regexp.MustCompile(`\n\s*\n+`)
	reDrop     = func() []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, t := range []string{"script", "style", "noscript", "svg", "template", "head"} {
			out = append(out, regexp.MustCompile(`(?is)<`+t+`\b.*?</`+t+`\s*>`))
		}
		return out
	}()
)

// htmlToText is a small, forgiving HTML to plain-text converter (no JavaScript, no layout).
func htmlToText(s string) (title, text string) {
	if m := reTitle.FindStringSubmatch(s); m != nil {
		title = strings.TrimSpace(reSpaces.ReplaceAllString(html.UnescapeString(reTags.ReplaceAllString(m[1], "")), " "))
	}
	s = reComments.ReplaceAllString(s, "")
	for _, re := range reDrop {
		s = re.ReplaceAllString(s, "")
	}
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return title, strings.TrimSpace(s)
}

// searxngSearch queries a SearXNG instance (JSON output must be enabled in its settings).
func (a *App) searxngSearch(ctx context.Context, q string) (string, error) {
	u, err := url.Parse(a.cfg.SearxngURL + "/search")
	if err != nil {
		return "", errors.New("SEARXNG_URL 不合法")
	}
	u.RawQuery = url.Values{"q": {q}, "format": {"json"}}.Encode()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return "", errors.New("无法连接 SearXNG")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch resp.StatusCode {
	case 200:
	case 403:
		return "", errors.New("SearXNG 拒绝了 JSON 请求：请在它的 settings.yml 里把 json 加入 search.formats")
	case 429:
		return "", errors.New("SearXNG 限流了这次请求")
	default:
		return "", fmt.Errorf("SearXNG 返回 HTTP %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			Engine        string `json:"engine"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", errors.New("SearXNG 返回的不是 JSON")
	}
	if len(out.Results) == 0 {
		return "没有搜索结果", nil
	}
	var b strings.Builder
	for i, r := range out.Results {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "%d. %s (%s %s)\n%s\n%s\n\n", i+1, r.Title, r.Engine, r.PublishedDate, r.URL, truncate(r.Content, 500))
	}
	return strings.TrimSpace(b.String()), nil
}
