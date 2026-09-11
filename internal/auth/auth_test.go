package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

// TestLoadDirLoadsAllValid 不再按 region 过滤：所有可解析的 auth 文件都被加载，
// 解析失败的文件静默跳过。
func TestLoadDirLoadsAllValid(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	other := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"example.com"},"account":{"uid":"u2"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(other), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 valid accounts, got %+v", list)
	}
	for _, a := range list {
		if a.FilePath == "" {
			t.Error("FilePath not set")
		}
	}
}

// TestFilterRegion 一区一实例：只保留指定区域的账号；空 region 原样返回。
func TestFilterRegion(t *testing.T) {
	cn1 := &Auth{UID: "cn1", Domain: "copilot.tencent.com"}
	cn2 := &Auth{UID: "cn2", Domain: ""}
	gl1 := &Auth{UID: "gl1", Domain: "www.workbuddy.ai"}
	gl2 := &Auth{UID: "gl2", Domain: "codebuddy.ai"}
	all := []*Auth{cn1, cn2, gl1, gl2}

	got := FilterRegion(all, RegionCN)
	if len(got) != 2 || got[0] != cn1 || got[1] != cn2 {
		t.Errorf("cn filter: got %v", uids(got))
	}
	got = FilterRegion(all, RegionGlobal)
	if len(got) != 2 || got[0] != gl1 || got[1] != gl2 {
		t.Errorf("global filter: got %v", uids(got))
	}
	// 空 region = 不限定，必须原样返回（含 nil）。
	if got := FilterRegion(all, ""); len(got) != 4 {
		t.Errorf("empty region should keep all, got %v", uids(got))
	}
	if got := FilterRegion(nil, ""); got != nil {
		t.Errorf("nil input with empty region should stay nil, got %v", got)
	}
	// 无匹配账户 → 空结果（不是 nil 崩溃）。
	if got := FilterRegion([]*Auth{cn1}, RegionGlobal); len(got) != 0 {
		t.Errorf("no match should yield empty, got %v", uids(got))
	}
}

func uids(as []*Auth) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.UID)
	}
	return out
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}

// TestRegion 域名 → 区域判定：workbuddy.ai / codebuddy.ai 及其子域属国际站，
// 空 domain 与 CN 域名归 CN（空 domain 是 CN 凭证的历史形态，必须向后兼容）。
func TestRegion(t *testing.T) {
	globals := []string{
		"workbuddy.ai", "www.workbuddy.ai", "api.workbuddy.ai", "WorkBuddy.AI",
		"codebuddy.ai", "www.codebuddy.ai", "  www.workbuddy.ai  ",
	}
	for _, d := range globals {
		if got := (&Auth{Domain: d}).Region(); got != RegionGlobal {
			t.Errorf("domain %q: got %s want global", d, got)
		}
	}
	cns := []string{"", "copilot.tencent.com", "codebuddy.cn", "www.codebuddy.cn", "example.com"}
	for _, d := range cns {
		if got := (&Auth{Domain: d}).Region(); got != RegionCN {
			t.Errorf("domain %q: got %s want cn", d, got)
		}
	}
	// nil 接收者按 CN 处理，避免调用方到处判空。
	if got := (*Auth)(nil).Region(); got != RegionCN {
		t.Errorf("nil auth: got %s want cn", got)
	}
	// 不得把"恰好以 workbuddy.ai 结尾但不是其子域"的域名误判为国际站（如 evilworkbuddy.ai）。
	for _, d := range []string{"evilworkbuddy.ai", "notworkbuddy.ai"} {
		if got := (&Auth{Domain: d}).Region(); got != RegionCN {
			t.Errorf("domain %q: got %s want cn (must not match bare suffix)", d, got)
		}
	}
}
