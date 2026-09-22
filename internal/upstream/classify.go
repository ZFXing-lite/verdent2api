package upstream

import (
	"strings"
)

// Class 错误分类，驱动池状态机。
type Class string

const (
	// ClassOK 正常（未用）。
	ClassOK Class = "ok"
	// ClassRateLimited 429，短冷却后可复用。
	ClassRateLimited Class = "rate_limited"
	// ClassNoCredit 余额/额度不足，长冷却（通常按天）。
	ClassNoCredit Class = "no_credit"
	// ClassKeyDead 401/403 鉴权失败，key 已失效，需人工更换。
	ClassKeyDead Class = "key_dead"
	// ClassTransient 5xx 等可重试错误。
	ClassTransient Class = "transient"
	// ClassOther 未分类错误。
	ClassOther Class = "other"
)

// DefaultUserAgent 默认出站 UA。
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// classify 按状态码与正文把上游错误归类。
func classify(status int, body string) Class {
	low := strings.ToLower(body)
	switch {
	case status == 401 || status == 403:
		// 部分平台把「余额不足」也用 401 表达，二次确认正文。
		if isNoCreditText(low) {
			return ClassNoCredit
		}
		return ClassKeyDead
	case status == 402:
		return ClassNoCredit
	case status == 429:
		// 429 也可能是额度耗尽（非频控），正文命中按缺额处理。
		if isNoCreditText(low) {
			return ClassNoCredit
		}
		return ClassRateLimited
	case status >= 500:
		return ClassTransient
	default:
		if isNoCreditText(low) {
			return ClassNoCredit
		}
	}
	return ClassOther
}

// isNoCreditText 命中「额度/余额不足」的文本特征。
func isNoCreditText(low string) bool {
	keywords := []string{
		"insufficient", "quota", "exceeded", "no credit", "credits",
		"balance", "limit reached", "usage limit", "billing",
		"exhausted", "plan limit", "rate dollar",
	}
	for _, k := range keywords {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}
