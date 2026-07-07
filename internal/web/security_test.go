package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestRequest 构造一个带可选 Authorization 头与 token 查询参数的请求，供令牌校验测试使用。
func newTestRequest(auth, queryToken string) *http.Request {
	target := "/api/state"
	if queryToken != "" {
		target += "?token=" + queryToken
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"0.0.0.0:8080":   false,
		"192.168.1.9:80": false,
		"example.com:80": false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestHostAllowed_LoopbackBind(t *testing.T) {
	s := &Server{addr: "127.0.0.1:8080"}
	allowed := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"}
	for _, h := range allowed {
		if !s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = false, want true", h)
		}
	}
	// DNS rebinding：攻击者域名解析到本机，但 Host 头非回环 -> 拒绝
	denied := []string{"evil.com:8080", "attacker.example:8080"}
	for _, h := range denied {
		if s.hostAllowed(h) {
			t.Errorf("hostAllowed(%q) = true, want false (rebinding must be blocked)", h)
		}
	}
}

func TestHostAllowed_NonLoopbackBindWithToken(t *testing.T) {
	// 具体绑定主机放行其自身；通配绑定 + token 放行任意 Host（交由 token 鉴权）
	s := &Server{addr: "192.168.1.9:8080", token: "secret"}
	if !s.hostAllowed("192.168.1.9:8080") {
		t.Error("bind host should be allowed")
	}
	if s.hostAllowed("other.example:8080") {
		t.Error("non-bind, non-loopback host must be denied for specific bind")
	}

	wild := &Server{addr: "0.0.0.0:8080", token: "secret"}
	if !wild.hostAllowed("anything.example:8080") {
		t.Error("wildcard bind with token should allow any host (token guards it)")
	}
	wildNoToken := &Server{addr: "0.0.0.0:8080"}
	if wildNoToken.hostAllowed("anything.example:8080") {
		t.Error("wildcard bind without token must not blanket-allow hosts")
	}
}

func TestHostAllowed_ExtraAllowedHosts(t *testing.T) {
	s := &Server{addr: "127.0.0.1:8080", allowedHosts: []string{"proxy.internal"}}
	if !s.hostAllowed("proxy.internal:443") {
		t.Error("configured allowed host should pass")
	}
}

func TestOriginAllowed(t *testing.T) {
	s := &Server{addr: "127.0.0.1:8080"}
	// 同源
	if !s.originAllowed("http://127.0.0.1:8080", "127.0.0.1:8080") {
		t.Error("same-origin request must be allowed")
	}
	// 跨站 CSRF
	if s.originAllowed("http://evil.com", "127.0.0.1:8080") {
		t.Error("cross-site origin must be denied")
	}
	// 畸形 Origin
	if s.originAllowed("not-a-url", "127.0.0.1:8080") {
		t.Error("malformed origin must be denied")
	}
}

func TestTokenValid(t *testing.T) {
	s := &Server{token: "s3cret"}

	mk := func(auth, query string) bool {
		r := newTestRequest(auth, query)
		return s.tokenValid(r)
	}
	if !mk("Bearer s3cret", "") {
		t.Error("valid bearer token should pass")
	}
	if !mk("", "s3cret") {
		t.Error("valid query token should pass")
	}
	if mk("Bearer wrong", "") {
		t.Error("wrong bearer token must fail")
	}
	if mk("", "wrong") {
		t.Error("wrong query token must fail")
	}
	if mk("", "") {
		t.Error("missing token must fail")
	}
}

func TestParseAllowedHosts(t *testing.T) {
	got := parseAllowedHosts(" a.com , ,b.com,  ")
	if len(got) != 2 || got[0] != "a.com" || got[1] != "b.com" {
		t.Fatalf("parseAllowedHosts = %#v, want [a.com b.com]", got)
	}
	if parseAllowedHosts("") != nil {
		t.Error("empty input should yield nil")
	}
}
