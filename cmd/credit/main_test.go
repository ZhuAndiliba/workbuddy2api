package main

import "testing"

// TestBillingBaseFor 计费 host 按账号 domain 路由：国际站域名走 workbuddy.ai，
// 空值/未知域名回落 CN（向后兼容 CN 历史凭证不写 domain 的形态）。
func TestBillingBaseFor(t *testing.T) {
	globals := []string{"workbuddy.ai", "www.workbuddy.ai", "codebuddy.ai", "www.codebuddy.ai"}
	for _, d := range globals {
		if got := billingBaseFor(d); got != billingBaseGlobal {
			t.Errorf("domain %q: got %q want %q", d, got, billingBaseGlobal)
		}
	}
	cns := []string{"", "copilot.tencent.com", "www.codebuddy.cn", "example.com"}
	for _, d := range cns {
		if got := billingBaseFor(d); got != billingBaseCN {
			t.Errorf("domain %q: got %q want %q", d, got, billingBaseCN)
		}
	}
}
