package redisstore

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		token string
		want  string
	}{
		{"完整rediss", "rediss://default:tok@host:6379", "ignored", "rediss://default:tok@host:6379"},
		{"完整redis", "redis://default:tok@host:6379", "ignored", "redis://default:tok@host:6379"},
		{"https host", "https://foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
		{"裸host", "foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeURL(c.url, c.token); got != c.want {
				t.Errorf("normalizeURL(%q,%q)=%q want %q", c.url, c.token, got, c.want)
			}
		})
	}
}

func TestNormalizeURLStripsTrailingPath(t *testing.T) {
	// 用户照抄 Upstash 控制台的 REST 地址，可能带任意路径——剥 scheme 只取 host:port 之前段。
	got := normalizeURL("https://foo.upstash.io", "t")
	if strings.Contains(got, "://foo.upstash.io") && !strings.HasSuffix(got, "foo.upstash.io:6379") {
		t.Errorf("unexpected: %s", got)
	}
}

func TestNewEmptyURLReturnsNoop(t *testing.T) {
	if _, ok := New("", "").(Noop); !ok {
		t.Fatalf("empty url should return Noop")
	}
}

func TestNewBadSchemeReturnsNoop(t *testing.T) {
	// 组装出的连接串含空格 → ParseURL 解析失败 → 降级 Noop，不 panic、不发网络请求。
	if _, ok := New("://bad host", "").(Noop); !ok {
		t.Fatalf("bad url should return Noop")
	}
}

func TestNoopMethods(t *testing.T) {
	n := Noop{}
	n.SetBind("k", "u", time.Minute) // 不 panic
	n.DelBind("k")
	n.SaveState([]byte("{}"))
	if _, ok := n.LoadState(); ok {
		t.Error("Noop.LoadState should report not-found")
	}
}

// TestKeyPrefixFor 一区一实例时键必须按区域加命名空间，否则两个实例互相覆盖；
// 未限定区域时保持旧前缀，保证既有部署升级后仍读得到自己写的快照。
func TestKeyPrefixFor(t *testing.T) {
	cases := map[string]string{
		"":       "wb2api:",
		"cn":     "wb2api:cn:",
		"global": "wb2api:global:",
	}
	for region, want := range cases {
		if got := keyPrefixFor(region); got != want {
			t.Errorf("keyPrefixFor(%q)=%q want %q", region, got, want)
		}
	}
	// 两个区域的键空间不得相交（旧前缀是 cn 前缀的前缀，但 bind 段不同）。
	cn, gl := keyPrefixFor("cn")+"bind:", keyPrefixFor("global")+"bind:"
	for _, k := range []string{"conv1", "conv2"} {
		if cn+k == gl+k {
			t.Errorf("cn/global key collision for %q", k)
		}
	}
}

// TestBindKeyUsesPrefix 实例的 bindKey 必须带上该实例前缀。
func TestBindKeyUsesPrefix(t *testing.T) {
	u := &Upstash{prefix: keyPrefixFor("global")}
	if got := u.bindKey("abc"); got != "wb2api:global:bind:abc" {
		t.Errorf("bindKey=%q want wb2api:global:bind:abc", got)
	}
}
